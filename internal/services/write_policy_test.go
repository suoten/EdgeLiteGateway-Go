package services

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"edgelite/internal/drivers"
	"edgelite/internal/models"
	"edgelite/internal/storage"
)

// policyDriver is a device whose answers the test decides: it records writes and
// replies to the read-back with whatever the case needs, including a value that
// disagrees with the write.
type policyDriver struct {
	drivers.BaseDriver
	writeErr  error
	readValue interface{}
	readErr   error
	readQual  string
	readCalls int
	written   []interface{}
}

type typedPolicyDriver struct {
	policyDriver
	lastType string
}

func (d *typedPolicyDriver) WritePointTyped(ctx context.Context, point string, value interface{}, dataType string) error {
	d.lastType = dataType
	return d.WritePoint(ctx, point, value)
}

func newPolicyDriver() *policyDriver {
	d := &policyDriver{}
	d.SetDeviceID("policy")
	return d
}

func (d *policyDriver) Name() string { return "simulator" }

func (d *policyDriver) Connect(ctx context.Context) error {
	d.SetConnected(true)
	return nil
}

func (d *policyDriver) Disconnect() error { return nil }

func (d *policyDriver) Discover(ctx context.Context, config map[string]interface{}) ([]map[string]interface{}, error) {
	return nil, nil
}

func (d *policyDriver) HealthCheck(ctx context.Context) error { return nil }

func (d *policyDriver) WritePoint(ctx context.Context, point string, value interface{}) error {
	if d.writeErr != nil {
		return d.writeErr
	}
	d.written = append(d.written, value)
	return nil
}

func (d *policyDriver) ReadPoints(ctx context.Context, points []models.PointDef) ([]storage.PointData, error) {
	d.readCalls++
	if d.readErr != nil {
		return nil, d.readErr
	}
	quality := d.readQual
	if quality == "" {
		quality = "good"
	}
	return []storage.PointData{{
		DeviceID:  d.DeviceID(),
		PointName: points[0].Name,
		Value:     d.readValue,
		Quality:   quality,
		Timestamp: time.Now(),
	}}, nil
}

func TestWriteValuesMatch(t *testing.T) {
	cases := []struct {
		name     string
		dataType string
		want     interface{}
		got      interface{}
		match    bool
	}{
		{"float rounds within register precision", "float32", 20.1, 20.099998, true},
		{"float outside tolerance", "float32", 20.1, 19.5, false},
		{"integer must be exact", "int16", 7.0, 8.0, false},
		{"integer rounds the read back", "int16", 7.4, 7.0, true},
		{"integer written as json number", "int32", float64(7), int16(7), true},
		{"bool identical", "bool", true, true, true},
		{"bool flipped", "bool", true, false, false},
		{"string identical", "string", "AUTO", "AUTO", true},
		{"string differs", "string", "AUTO", "MANUAL", false},
		{"read back a non number for a float", "float32", 1.0, "NaN-ish", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := writeValuesMatch(tc.dataType, tc.want, tc.got); got != tc.match {
				t.Fatalf("writeValuesMatch(%q, %v, %v) = %v, want %v", tc.dataType, tc.want, tc.got, got, tc.match)
			}
		})
	}
}

func TestVerifyDeviceWrite(t *testing.T) {
	point := &models.PointDef{Name: "setpoint", DataType: "float32", Address: "0"}

	t.Run("matching read-back is a success", func(t *testing.T) {
		drv := newPolicyDriver()
		drv.readValue = 42.5
		if err := verifyDeviceWrite(context.Background(), drv, point, "setpoint", 42.5); err != nil {
			t.Fatalf("verify returned %v, want nil", err)
		}
	})

	t.Run("mismatch is an error naming both values", func(t *testing.T) {
		drv := newPolicyDriver()
		drv.readValue = 11.0
		err := verifyDeviceWrite(context.Background(), drv, point, "setpoint", 42.5)
		if !errors.Is(err, ErrWriteVerifyFailed) {
			t.Fatalf("verify returned %v, want ErrWriteVerifyFailed", err)
		}
		if !strings.Contains(err.Error(), "42.5") || !strings.Contains(err.Error(), "11") {
			t.Errorf("message %q must show the written and the read-back value", err.Error())
		}
		// The retry gives the device one more chance, so the read happens twice.
		if drv.readCalls != 2 {
			t.Errorf("read calls = %d, want the failed read to be retried once", drv.readCalls)
		}
	})

	t.Run("bad quality is an error", func(t *testing.T) {
		drv := newPolicyDriver()
		drv.readValue = 42.5
		drv.readQual = "bad"
		err := verifyDeviceWrite(context.Background(), drv, point, "setpoint", 42.5)
		if !errors.Is(err, ErrWriteVerifyFailed) {
			t.Fatalf("verify returned %v, want ErrWriteVerifyFailed", err)
		}
		if !strings.Contains(err.Error(), "quality") {
			t.Errorf("message %q should name the quality as the reason", err.Error())
		}
	})

	t.Run("nil value is an error", func(t *testing.T) {
		drv := newPolicyDriver()
		drv.readValue = nil
		err := verifyDeviceWrite(context.Background(), drv, point, "setpoint", 42.5)
		if !errors.Is(err, ErrWriteVerifyFailed) {
			t.Fatalf("verify returned %v, want ErrWriteVerifyFailed", err)
		}
	})

	t.Run("driver error is an error", func(t *testing.T) {
		drv := newPolicyDriver()
		drv.readErr = errors.New("modbus exception 0x02")
		err := verifyDeviceWrite(context.Background(), drv, point, "setpoint", 42.5)
		if !errors.Is(err, ErrWriteVerifyFailed) || !strings.Contains(err.Error(), "0x02") {
			t.Fatalf("verify returned %v, want the device error carried through", err)
		}
	})

	t.Run("missing point definition still reads the named point", func(t *testing.T) {
		drv := newPolicyDriver()
		drv.readValue = "AUTO"
		if err := verifyDeviceWrite(context.Background(), drv, nil, "mode", "AUTO"); err != nil {
			t.Fatalf("verify returned %v, want nil", err)
		}
	})
}

func TestEnforceWriteRateLimit(t *testing.T) {
	svc := NewDeviceService(nil, nil, nil, nil)
	cfg := map[string]interface{}{"write_rate_limit": 200}

	if err := svc.enforceWriteRateLimit("dev", "p", cfg); err != nil {
		t.Fatalf("first write returned %v, want admission", err)
	}
	err := svc.enforceWriteRateLimit("dev", "p", cfg)
	if !errors.Is(err, ErrWriteRateLimited) {
		t.Fatalf("second write returned %v, want ErrWriteRateLimited", err)
	}
	if !strings.Contains(err.Error(), "ms") {
		t.Errorf("message %q should tell the operator how long to wait", err.Error())
	}
	if err := svc.enforceWriteRateLimit("dev", "other", cfg); err != nil {
		t.Errorf("a different point must have its own budget, got %v", err)
	}
	if err := svc.enforceWriteRateLimit("dev2", "p", cfg); err != nil {
		t.Errorf("another device must have its own budget, got %v", err)
	}

	time.Sleep(220 * time.Millisecond)
	if err := svc.enforceWriteRateLimit("dev", "p", cfg); err != nil {
		t.Errorf("write after the window returned %v, want admission", err)
	}
}

func TestWriteRateLimitDisabled(t *testing.T) {
	svc := NewDeviceService(nil, nil, nil, nil)
	for _, cfg := range []map[string]interface{}{
		nil,
		{},
		{"write_rate_limit": 0},
		{"write_rate_limit": -5},
		{"write_rate_limit": "300"},
	} {
		for i := 0; i < 5; i++ {
			if err := svc.enforceWriteRateLimit("dev", "p", cfg); err != nil {
				t.Fatalf("config %v write %d returned %v, want unlimited writes", cfg, i, err)
			}
		}
	}
}

func TestWriteRateLimitPrunesStaleEntries(t *testing.T) {
	svc := NewDeviceService(nil, nil, nil, nil)
	cfg := map[string]interface{}{"write_rate_limit": 50}

	// Fill the map past the threshold with timestamps that can never gate a
	// write again, plus one that still can.
	for i := 0; i < writeTimesPruneThreshold; i++ {
		svc.lastWrite[fmt.Sprintf("dev\x00p%d", i)] = time.Now().Add(-time.Second)
	}
	fresh := time.Now().Add(-10 * time.Millisecond)
	svc.lastWrite["dev\x00fresh"] = fresh

	if err := svc.enforceWriteRateLimit("dev", "target", cfg); err != nil {
		t.Fatalf("admission returned %v", err)
	}
	if len(svc.lastWrite) != 2 {
		t.Fatalf("throttle map holds %d entries, want the stale ones pruned", len(svc.lastWrite))
	}
	if !svc.lastWrite["dev\x00fresh"].Equal(fresh) {
		t.Error("pruning dropped an entry that is still inside its window")
	}
	if err := svc.enforceWriteRateLimit("dev", "fresh", cfg); !errors.Is(err, ErrWriteRateLimited) {
		t.Errorf("throttling of the surviving entry returned %v, want ErrWriteRateLimited", err)
	}
}

func TestWriteRateLimitDuration(t *testing.T) {
	if got := writeRateLimit(map[string]interface{}{"write_rate_limit": 500}); got != 500*time.Millisecond {
		t.Errorf("writeRateLimit = %v, want 500ms (the documented unit)", got)
	}
	if got := writeRateLimit(map[string]interface{}{"write_rate_limit": float64(1500)}); got != 1500*time.Millisecond {
		t.Errorf("writeRateLimit = %v, want 1.5s", got)
	}
	if got := writeRateLimit(map[string]interface{}{}); got != 0 {
		t.Errorf("writeRateLimit = %v, want 0 when unset", got)
	}
}

func TestCheckWriteWhitelist(t *testing.T) {
	ctx := context.Background()
	withActor := WithWriteActor(ctx, WriteActor{UserID: "u2", Username: "ops", Role: "operator"})
	admin := WithWriteActor(ctx, WriteActor{UserID: "u1", Username: "root", Role: "admin"})

	if err := checkWriteWhitelist(ctx, nil); err != nil {
		t.Errorf("no config returned %v, want no restriction", err)
	}
	if err := checkWriteWhitelist(ctx, map[string]interface{}{}); err != nil {
		t.Errorf("empty config returned %v, want no restriction", err)
	}
	if err := checkWriteWhitelist(withActor, map[string]interface{}{"write_whitelist": []interface{}{"ops"}}); err != nil {
		t.Errorf("listed user returned %v, want admission", err)
	}
	if err := checkWriteWhitelist(withActor, map[string]interface{}{"write_whitelist": []interface{}{"u2"}}); err != nil {
		t.Errorf("user id match returned %v, want admission", err)
	}
	if err := checkWriteWhitelist(admin, map[string]interface{}{"write_whitelist": []interface{}{"someone-else"}}); err != nil {
		t.Errorf("admin returned %v, want the role to bypass the list", err)
	}

	err := checkWriteWhitelist(withActor, map[string]interface{}{"write_whitelist": []interface{}{"only-this-user"}})
	if !errors.Is(err, ErrWriteNotAllowed) {
		t.Fatalf("unlisted user returned %v, want ErrWriteNotAllowed", err)
	}
	if !strings.Contains(err.Error(), "ops") {
		t.Errorf("message %q should name who was refused", err.Error())
	}
	// An unauthenticated caller cannot be checked against a list, so the write is
	// refused rather than treated as anonymous-and-allowed.
	if err := checkWriteWhitelist(ctx, map[string]interface{}{"write_whitelist": []interface{}{"ops"}}); !errors.Is(err, ErrWriteNotAllowed) {
		t.Errorf("no identity returned %v, want ErrWriteNotAllowed", err)
	}
}

func TestConfigStringList(t *testing.T) {
	cases := map[string]interface{}{
		"list":      []interface{}{"a", "", 3, "b"},
		"strings":   []string{"a", "b"},
		"commas":    "a, b;c\n d",
		"semicolon": ";",
	}
	for name, value := range cases {
		got := configStringList(map[string]interface{}{"k": value}, "k")
		t.Log(name, got)
	}
	if got := configStringList(map[string]interface{}{"k": "a, b;c\n d"}, "k"); len(got) != 4 || got[3] != "d" {
		t.Errorf("string whitelist parsed as %v", got)
	}
	if got := configStringList(map[string]interface{}{"k": []interface{}{"a", "", 3, "b"}}, "k"); len(got) != 2 {
		t.Errorf("mixed list parsed as %v, want the two usable names", got)
	}
	if got := configStringList(nil, "k"); got != nil {
		t.Errorf("nil config parsed as %v", got)
	}
	if got := configStringList(map[string]interface{}{"k": 7}, "k"); got != nil {
		t.Errorf("non-list value parsed as %v", got)
	}
}

func TestWriteActorContextRoundTrip(t *testing.T) {
	ctx := context.Background()
	if _, ok := WriteActorFromContext(ctx); ok {
		t.Fatal("empty context reported an actor")
	}
	ctx = WithWriteActor(ctx, WriteActor{Username: "  "})
	if _, ok := WriteActorFromContext(ctx); ok {
		t.Fatal("a blank username is not an identity")
	}
	ctx = WithWriteActor(context.Background(), WriteActor{UserID: "u1", Username: "ops", Role: "operator"})
	actor, ok := WriteActorFromContext(ctx)
	if !ok || actor.Username != "ops" || actor.Role != "operator" {
		t.Fatalf("round trip returned %v %v", actor, ok)
	}
}

func TestCheckPointWritableAndPointLookup(t *testing.T) {
	dev := &models.DeviceResponse{Points: []models.PointDef{
		{Name: "rw", AccessMode: "rw"},
		{Name: "ro", AccessMode: "READ"},
		{Name: "unspecified"},
	}}
	if err := checkPointWritable(dev, "rw"); err != nil {
		t.Errorf("read-write point returned %v", err)
	}
	if err := checkPointWritable(dev, "ro"); !errors.Is(err, ErrReadOnlyPoint) {
		t.Errorf("read-only point returned %v, want ErrReadOnlyPoint", err)
	}
	if err := checkPointWritable(dev, "unspecified"); err != nil {
		t.Errorf("point without access_mode returned %v, want the driver to decide", err)
	}
	if err := checkPointWritable(nil, "anything"); err != nil {
		t.Errorf("unknown device returned %v, want the driver to decide", err)
	}
	if p := pointDefOf(dev, "rw"); p == nil || p.Name != "rw" {
		t.Errorf("pointDefOf returned %v", p)
	}
	if p := pointDefOf(dev, "missing"); p != nil {
		t.Errorf("pointDefOf returned %v for an unknown point, want nil", p)
	}
	if p := pointDefOf(nil, "rw"); p != nil {
		t.Errorf("pointDefOf returned %v for a missing device, want nil", p)
	}
}

// WritePoint is the single funnel: the whitelist, the throttle, the driver write
// and the read-back have to happen in that order, and a write the device rejects
// must not be verified as if it had succeeded.
func TestWritePointFunnel(t *testing.T) {
	svc := NewDeviceService(nil, nil, nil, nil)
	drv := newPolicyDriver()
	drv.readValue = 9.0
	svc.drivers["dev"] = drv

	cfg := map[string]interface{}{"write_verify": true}
	if err := verifyDeviceWrite(context.Background(), drv, &models.PointDef{Name: "p", DataType: "float32"}, "p", 9.0); err != nil {
		t.Fatalf("echoed value returned %v", err)
	}
	if err := verifyDeviceWrite(context.Background(), drv, &models.PointDef{Name: "p", DataType: "float32"}, "p", 3.0); !errors.Is(err, ErrWriteVerifyFailed) {
		t.Fatalf("mismatch returned %v, want ErrWriteVerifyFailed", err)
	}
	if configBool(cfg, "write_verify") != true || configBool(map[string]interface{}{}, "write_verify") {
		t.Error("configBool misread the write_verify flag")
	}
}

// The typed write path must survive the funnel refactor: Modbus register pairs
// need the declared data type to pick their word layout.
func TestWriteDriverPointPassesDataType(t *testing.T) {
	drv := &typedPolicyDriver{}
	drv.SetDeviceID("typed")
	drv.SetConnected(true)
	if err := writeDriverPoint(context.Background(), drv, "p", 1.0, "int32", ""); err != nil {
		t.Fatalf("typed write returned %v", err)
	}
	if drv.lastType != "int32" {
		t.Fatalf("driver received data type %q, want int32", drv.lastType)
	}

	plain := newPolicyDriver()
	if err := writeDriverPoint(context.Background(), plain, "p", 1.0, "int32", ""); err != nil {
		t.Fatalf("plain write returned %v", err)
	}
	if len(plain.written) != 1 {
		t.Errorf("plain driver recorded %d writes", len(plain.written))
	}
}
