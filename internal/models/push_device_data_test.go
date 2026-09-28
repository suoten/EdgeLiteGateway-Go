package models

import (
	"encoding/json"
	"testing"
)

// ProtoForge's HTTP push loop (integration manager _http_push_loop) posts the
// Python edition's dict contract {"data": {point: {value, quality, timestamp}}}
// to POST /devices/{id}/push, while this edition's own callers post the array
// form and webhook clients post flat scalars. One decoder must accept all
// three or a silent push contract break shows up as "empty push" 400s.

func TestPushDeviceDataRequestDictOfObjects(t *testing.T) {
	raw := `{"device_id":"dev-1","data":{"temperature":{"value":25.5,"quality":"good","timestamp":"2026-09-27T12:00:00Z"},"humidity":{"value":60,"quality":"good"}}}`
	var req PushDeviceDataRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("unmarshal dict payload: %v", err)
	}
	if len(req.Data) != 2 {
		t.Fatalf("data entries = %d, want 2", len(req.Data))
	}
	byPoint := map[string]PushDataPointValue{}
	for _, p := range req.Data {
		byPoint[p.Point] = p
	}
	temp, ok := byPoint["temperature"]
	if !ok || temp.Value != 25.5 || temp.Quality != "good" || temp.Timestamp != "2026-09-27T12:00:00Z" {
		t.Fatalf("temperature entry = %+v", temp)
	}
	hum, ok := byPoint["humidity"]
	if !ok || hum.Value != float64(60) || hum.Quality != "good" {
		t.Fatalf("humidity entry = %+v", hum)
	}
}

func TestPushDeviceDataRequestArrayOfObjects(t *testing.T) {
	raw := `{"device_id":"dev-1","data":[{"point":"pressure","value":101.3,"quality":"good","timestamp":"2026-09-27T12:00:00Z"}]}`
	var req PushDeviceDataRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("unmarshal array payload: %v", err)
	}
	if len(req.Data) != 1 || req.Data[0].Point != "pressure" || req.Data[0].Value != 101.3 {
		t.Fatalf("data = %+v", req.Data)
	}
}

func TestPushDeviceDataRequestFlatScalarMap(t *testing.T) {
	raw := `{"device_id":"dev-1","data":{"temperature":25.5,"running":true}}`
	var req PushDeviceDataRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("unmarshal scalar map: %v", err)
	}
	if len(req.Data) != 2 {
		t.Fatalf("data entries = %d, want 2", len(req.Data))
	}
	byPoint := map[string]interface{}{}
	for _, p := range req.Data {
		byPoint[p.Point] = p.Value
	}
	if byPoint["temperature"] != 25.5 || byPoint["running"] != true {
		t.Fatalf("scalar map = %+v", byPoint)
	}
}

// Empty data decodes without error (the map branch simply produces no
// entries); the push HANDLER owns the "empty push" rejection via len(Data)==0,
// so the decoder contract here is only that nothing panics or half-fills.
func TestPushDeviceDataRequestEmptyDataYieldsNoEntries(t *testing.T) {
	// A present-but-empty data decodes to zero entries (handler then rejects
	// on len(Data)==0); a MISSING data key is a decode error.
	for _, raw := range []string{`{"device_id":"dev-1","data":{}}`, `{"device_id":"dev-1","data":[]}`} {
		var req PushDeviceDataRequest
		if err := json.Unmarshal([]byte(raw), &req); err != nil {
			t.Errorf("decode %s: %v", raw, err)
		}
		if len(req.Data) != 0 {
			t.Errorf("data entries = %d, want 0 for %s", len(req.Data), raw)
		}
	}
	if err := json.Unmarshal([]byte(`{"device_id":"dev-1"}`), &PushDeviceDataRequest{}); err == nil {
		t.Error("missing data key must be a decode error")
	}
}
