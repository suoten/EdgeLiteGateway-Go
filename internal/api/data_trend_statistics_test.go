package api

import (
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"testing"
	"time"

	"edgelite/internal/storage"

	"github.com/labstack/echo/v4"
)

// GET /data/trend and GET /data/statistics used to answer a canned empty trend
// and all-zero statistics, so "the gateway holds no samples" and "nobody
// implemented this" were indistinguishable on a chart. Both now summarise the
// stored history, which is what the analytics page plots.

func callDataRoute(t *testing.T, h echo.HandlerFunc, path string) (int, string, map[string]any) {
	t.Helper()
	c, rec := setupEcho(http.MethodGet, path, "")
	if err := h(c); err != nil {
		t.Fatalf("handler returned an error: %v", err)
	}
	var env struct {
		ErrorCode string         `json:"error_code"`
		Data      map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unparseable body %s: %v", rec.Body.String(), err)
	}
	return rec.Code, env.ErrorCode, env.Data
}

// writeSeries stores samples at the given minute offsets from base for one tag.
func writeSeries(t *testing.T, tsDB *storage.TimeSeriesStorage, device, point string, base time.Time, offsets []int, values []any) {
	t.Helper()
	recs := make([]storage.PointData, 0, len(offsets))
	for i, off := range offsets {
		recs = append(recs, storage.PointData{
			DeviceID: device, PointName: point, Value: values[i], Quality: "good",
			Timestamp: base.Add(time.Duration(off) * time.Minute),
		})
	}
	if err := tsDB.WritePoints(recs); err != nil {
		t.Fatalf("WritePoints: %v", err)
	}
}

func windowQuery(device, point string, start, stop time.Time, extra string) string {
	return "/api/v1/data/trend?device_id=" + device + "&point_name=" + point +
		"&start=" + urlEncodeRFC3339(start) + "&stop=" + urlEncodeRFC3339(stop) + extra
}

func urlEncodeRFC3339(t time.Time) string {
	return url.QueryEscape(t.Format(time.RFC3339))
}

func TestQueryTrendBucketsStoredSamples(t *testing.T) {
	tsDB, _ := useQualityStore(t)
	base := time.Now().Add(-30 * time.Minute).Truncate(time.Minute)
	writeSeries(t, tsDB, "dev-tr", "temp", base, []int{0, 1, 2, 7, 8, 12},
		[]any{1.0, 2.0, 3.0, 4.0, 5.0, 6.0})
	// Outside the requested window on both sides: neither may move a bucket.
	writeSeries(t, tsDB, "dev-tr", "temp", base, []int{-60, 60}, []any{999.0, 999.0})

	code, ec, data := callDataRoute(t, handleQueryTrend, windowQuery("dev-tr", "temp", base, base.Add(15*time.Minute), "&bucket_size=5m"))
	if code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", code, ec)
	}
	if got, _ := data["sample_count"].(float64); got != 6 {
		t.Fatalf("sample_count = %v, want 6 (the two outside-window samples are excluded)", data["sample_count"])
	}
	buckets, _ := data["trend"].([]any)
	if len(buckets) != 3 {
		t.Fatalf("trend buckets = %d, want 3 (%v)", len(buckets), data["trend"])
	}
	want := []struct {
		n             int
		avg, min, max float64
	}{
		{3, 2.0, 1.0, 3.0},
		{2, 4.5, 4.0, 5.0},
		{1, 6.0, 6.0, 6.0},
	}
	for i, w := range want {
		b, _ := buckets[i].(map[string]any)
		if n, _ := b["count"].(float64); int(n) != w.n {
			t.Fatalf("bucket %d count = %v, want %d", i, b["count"], w.n)
		}
		for _, c := range []struct {
			key  string
			want float64
		}{{"avg", w.avg}, {"min", w.min}, {"max", w.max}} {
			if v, _ := b[c.key].(float64); math.Abs(v-c.want) > 1e-9 {
				t.Fatalf("bucket %d %s = %v, want %v", i, c.key, b[c.key], c.want)
			}
		}
	}
	// The bucket boundaries have to be the window's own grid, not sample times.
	first, _ := buckets[0].(map[string]any)
	if got, _ := first["start"].(string); got != base.Format(time.RFC3339) {
		t.Fatalf("bucket 0 start = %q, want %q", got, base.Format(time.RFC3339))
	}
}

// A gap in collection must stay a gap: filling it with the neighbouring value
// would draw a line the gateway never measured.
func TestQueryTrendLeavesEmptyBucketsOut(t *testing.T) {
	tsDB, _ := useQualityStore(t)
	base := time.Now().Add(-30 * time.Minute).Truncate(time.Minute)
	writeSeries(t, tsDB, "dev-gap", "temp", base, []int{0, 12}, []any{1.0, 2.0})

	_, _, data := callDataRoute(t, handleQueryTrend, windowQuery("dev-gap", "temp", base, base.Add(15*time.Minute), "&bucket_size=5m"))
	buckets, _ := data["trend"].([]any)
	if len(buckets) != 2 {
		t.Fatalf("trend buckets = %d, want 2 (the empty 5-10 min bucket stays out): %v", len(buckets), data["trend"])
	}
	second, _ := buckets[1].(map[string]any)
	if got, _ := second["start"].(string); got != base.Add(10*time.Minute).Format(time.RFC3339) {
		t.Fatalf("second bucket start = %q, want the 10-minute grid boundary", got)
	}
}

func TestQueryTrendNeedsWindowAndStore(t *testing.T) {
	useQualityStore(t)

	if code, _, _ := callDataRoute(t, handleQueryTrend, "/api/v1/data/trend?device_id=d&point_name=temp"); code != http.StatusBadRequest {
		t.Fatalf("missing window status = %d, want 400", code)
	}
	if code, _, _ := callDataRoute(t, handleQueryTrend, "/api/v1/data/trend?device_id=d"); code != http.StatusBadRequest {
		t.Fatalf("missing point_name status = %d, want 400", code)
	}

	cont := GetContainer()
	prev := cont.TsStorage
	cont.TsStorage = nil
	t.Cleanup(func() { cont.TsStorage = prev })
	now := time.Now()
	code, ec, _ := callDataRoute(t, handleQueryTrend, windowQuery("d", "temp", now.Add(-time.Hour), now, ""))
	if code != http.StatusServiceUnavailable || ec != "ERR_TS_STORAGE_UNAVAILABLE" {
		t.Fatalf("no-store status = %d error_code = %q, want 503/ERR_TS_STORAGE_UNAVAILABLE", code, ec)
	}
}

func TestGetDataStatisticsComputesAggregates(t *testing.T) {
	tsDB, _ := useQualityStore(t)
	base := time.Now().Add(-30 * time.Minute).Truncate(time.Minute)
	offsets := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}
	values := []any{1.0, 2.0, 3.0, 4.0, 5.0, 6.0, 7.0, 8.0, 9.0, 10.0}
	writeSeries(t, tsDB, "dev-st", "temp", base, offsets, values)
	// A text sample must not be averaged in as 0.
	writeSeries(t, tsDB, "dev-st", "temp", base, []int{10}, []any{"not-a-number"})

	q := "/api/v1/data/statistics?device_id=dev-st&point_name=temp&start=" + urlEncodeRFC3339(base) +
		"&stop=" + urlEncodeRFC3339(base.Add(15*time.Minute))
	code, ec, data := callDataRoute(t, handleGetDataStatistics, q)
	if code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", code, ec)
	}
	if got, _ := data["count"].(float64); got != 10 {
		t.Fatalf("count = %v, want 10 numeric samples", data["count"])
	}
	if got, _ := data["non_numeric_samples"].(float64); got != 1 {
		t.Fatalf("non_numeric_samples = %v, want 1", data["non_numeric_samples"])
	}
	for _, c := range []struct {
		key  string
		want float64
	}{
		{"mean", 5.5}, {"min", 1}, {"max", 10},
		// Sample standard deviation of 1..10 is sqrt(82.5/9).
		{"stddev", 3.0276503540974917},
		{"median", 5}, {"q1", 3}, {"q3", 8}, {"p95", 10},
	} {
		v, ok := data[c.key].(float64)
		if !ok {
			t.Fatalf("%s = %#v, want %v", c.key, data[c.key], c.want)
		}
		if math.Abs(v-c.want) > 1e-9 {
			t.Fatalf("%s = %v, want %v", c.key, v, c.want)
		}
	}
}

func TestGetDataStatisticsReportsNullForUnmeasured(t *testing.T) {
	tsDB, _ := useQualityStore(t)
	base := time.Now().Add(-30 * time.Minute).Truncate(time.Minute)
	q := func(point string) map[string]any {
		path := "/api/v1/data/statistics?device_id=dev-null&point_name=" + point +
			"&start=" + urlEncodeRFC3339(base) + "&stop=" + urlEncodeRFC3339(base.Add(15*time.Minute))
		code, ec, data := callDataRoute(t, handleGetDataStatistics, path)
		if code != http.StatusOK {
			t.Fatalf("status = %d (%s), want 200", code, ec)
		}
		return data
	}

	empty := q("absent")
	if got, _ := empty["count"].(float64); got != 0 {
		t.Fatalf("count = %v, want 0 for a tag with no samples", empty["count"])
	}
	for _, key := range []string{"mean", "stddev", "min", "max", "median", "p95"} {
		if v, ok := empty[key]; !ok || v != nil {
			t.Fatalf("%s = %#v, want null (nothing was measured)", key, v)
		}
	}

	writeSeries(t, tsDB, "dev-null", "single", base, []int{0}, []any{42.0})
	one := q("single")
	if got, _ := one["count"].(float64); got != 1 {
		t.Fatalf("count = %v, want 1", one["count"])
	}
	if v, _ := one["mean"].(float64); v != 42 {
		t.Fatalf("mean = %v, want the single sample", one["mean"])
	}
	if v, ok := one["stddev"]; !ok || v != nil {
		t.Fatalf("stddev = %#v, want null: one sample carries no spread", one["stddev"])
	}
}
