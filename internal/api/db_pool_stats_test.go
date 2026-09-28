package api

import (
	"net/http"
	"testing"
)

// The db-monitor pool stats handler returned five literals copied from the Python
// implementation, so a gateway whose SQLite pool was saturated and queuing every
// collect read looked exactly like an idle one.
func TestDBPoolStatsReportsTheLivePool(t *testing.T) {
	withDeviceService(t)

	code, env := callDeviceHandler(t, handleGetDBPoolStats, http.MethodGet,
		"/api/v1/db-monitor/pool-stats", "", nil, nil)
	if code != http.StatusOK {
		t.Fatalf("pool stats returned %d (%v)", code, env)
	}
	data, _ := env["data"].(map[string]interface{})
	// storage.NewDatabase uses a 15-connection ceiling when the config leaves
	// both pool sizes at zero, which is what the test container does. The literal
	// this handler used to return was 25.
	if data["max_open"] != float64(15) {
		t.Fatalf("max_open = %v, want the ceiling the pool was actually configured with", data["max_open"])
	}
	open, _ := data["open"].(float64)
	if open < 1 {
		t.Fatalf("open = %v, want at least the connection the handler just used for a read", open)
	}
	inUse, _ := data["in_use"].(float64)
	idle, _ := data["idle"].(float64)
	if inUse+idle > open {
		t.Fatalf("in_use %v + idle %v exceeds open %v: the numbers are not one pool's stats", inUse, idle, open)
	}
	if _, ok := data["wait_count"]; !ok {
		t.Fatal("wait_count missing; queued connection waits are the point of this endpoint")
	}
}

func TestDBPoolStatsWithoutADatabaseIsHonest(t *testing.T) {
	prev := GetContainer()
	SetContainer(&ServiceContainer{})
	t.Cleanup(func() { SetContainer(prev) })

	code, env := callDeviceHandler(t, handleGetDBPoolStats, http.MethodGet,
		"/api/v1/db-monitor/pool-stats", "", nil, nil)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("pool stats with no database returned %d (%v), want 503", code, env)
	}
	if env["error_code"] != "ERR_DB_UNAVAILABLE" {
		t.Fatalf("error_code = %v, want ERR_DB_UNAVAILABLE", env["error_code"])
	}
}
