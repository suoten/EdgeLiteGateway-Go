package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// UpdateSourceData used to compute converted values and drop them, counting each
// one as a transfer. The counters now move only when the write sink accepts the
// value, which is what these tests pin down.

func TestBridgeCountsOnlyRealWrites(t *testing.T) {
	mgr := NewProtocolBridgeManager()
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	var mu sync.Mutex
	var written []string
	mgr.SetWriteSink(func(_ context.Context, deviceID, point string, value interface{}) error {
		mu.Lock()
		defer mu.Unlock()
		written = append(written, deviceID+"."+point+"="+fmt.Sprintf("%v", value))
		return nil
	})

	bridge := NewProtocolBridge("pb1", "line", "src", "dst")
	bridge.AddRule(&MappingRule{RuleID: "r1", SourceDeviceID: "src", SourcePoint: "temp",
		TargetDeviceID: "dst", TargetPoint: "setpoint", ConversionType: "linear", Scale: 2, Offset: 1, Enabled: true})
	mgr.AddBridge(bridge)

	if err := mgr.UpdateSourceData(context.Background(), "src", "temp", 10.0); err != nil {
		t.Fatalf("UpdateSourceData: %v", err)
	}
	mu.Lock()
	got := append([]string(nil), written...)
	mu.Unlock()
	if len(got) != 1 || got[0] != "dst.setpoint=21" {
		t.Fatalf("sink saw %v, want one write of dst.setpoint=21", got)
	}
	stats := mgr.GetBridgeStats("pb1")
	if stats["total_transferred"] != int64(1) || stats["total_errors"] != int64(0) {
		t.Fatalf("stats after a real write: %#v", stats)
	}
}

func TestBridgeRecordsWriteFailure(t *testing.T) {
	mgr := NewProtocolBridgeManager()
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	mgr.SetWriteSink(func(context.Context, string, string, interface{}) error {
		return errors.New("ERR_DEVICE_WRITE_FAILED")
	})
	bridge := NewProtocolBridge("pb1", "line", "src", "dst")
	bridge.AddRule(&MappingRule{RuleID: "r1", SourceDeviceID: "src", SourcePoint: "temp",
		TargetDeviceID: "dst", TargetPoint: "setpoint", ConversionType: "passthrough", Enabled: true})
	mgr.AddBridge(bridge)

	if err := mgr.UpdateSourceData(context.Background(), "src", "temp", 3.5); err != nil {
		t.Fatalf("UpdateSourceData: %v", err)
	}
	stats := mgr.GetBridgeStats("pb1")
	if stats["total_transferred"] != int64(0) {
		t.Fatalf("a failed write counted as transferred: %#v", stats)
	}
	if stats["total_errors"] != int64(1) {
		t.Fatalf("a failed write was not counted: %#v", stats)
	}
	last, _ := stats["last_error"].(string)
	if last == "" {
		t.Fatalf("stats keep no reason for the failure: %#v", stats)
	}
}

func TestBridgeStopsWhenDisabledOrUnstarted(t *testing.T) {
	cases := []struct {
		name     string
		bridgeOn bool
		startMgr bool
	}{
		{"disabled bridge", false, true},
		{"stopped manager", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr := NewProtocolBridgeManager()
			if tc.startMgr {
				if err := mgr.Start(context.Background()); err != nil {
					t.Fatalf("Start: %v", err)
				}
			}
			var writes int
			mgr.SetWriteSink(func(context.Context, string, string, interface{}) error { writes++; return nil })
			bridge := NewProtocolBridge("pb1", "line", "src", "dst")
			bridge.SetEnabled(tc.bridgeOn)
			bridge.AddRule(&MappingRule{RuleID: "r1", SourceDeviceID: "src", SourcePoint: "temp",
				TargetDeviceID: "dst", TargetPoint: "setpoint", Enabled: true})
			mgr.AddBridge(bridge)

			if err := mgr.UpdateSourceData(context.Background(), "src", "temp", 1.0); err != nil {
				t.Fatalf("UpdateSourceData: %v", err)
			}
			if writes != 0 {
				t.Fatalf("%s still wrote %d values", tc.name, writes)
			}
		})
	}
}

func TestBridgeWithoutSinkReportsWhy(t *testing.T) {
	mgr := NewProtocolBridgeManager()
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	bridge := NewProtocolBridge("pb1", "line", "src", "dst")
	bridge.AddRule(&MappingRule{RuleID: "r1", SourceDeviceID: "src", SourcePoint: "temp",
		TargetDeviceID: "dst", TargetPoint: "setpoint", Enabled: true})
	mgr.AddBridge(bridge)

	if mgr.SinkWired() {
		t.Fatalf("a fresh manager should not claim a write sink")
	}
	if err := mgr.UpdateSourceData(context.Background(), "src", "temp", 1.0); err != nil {
		t.Fatalf("UpdateSourceData: %v", err)
	}
	stats := mgr.GetBridgeStats("pb1")
	if stats["total_transferred"] != int64(0) || stats["total_errors"] != int64(1) {
		t.Fatalf("a dropped value was not accounted for: %#v", stats)
	}
}

func TestRemoveBridgeReportsExistence(t *testing.T) {
	mgr := NewProtocolBridgeManager()
	mgr.AddBridge(NewProtocolBridge("pb1", "line", "src", "dst"))
	if !mgr.RemoveBridge("pb1") {
		t.Fatalf("RemoveBridge said the known bridge was absent")
	}
	if mgr.RemoveBridge("pb1") {
		t.Fatalf("RemoveBridge claimed to delete a bridge that was already gone")
	}
	if mgr.Count() != 0 {
		t.Fatalf("Count = %d after removing the only bridge", mgr.Count())
	}
}
