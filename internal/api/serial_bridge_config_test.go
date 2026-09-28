package api

// PUT /serial-bridge/config stored whatever it was handed and answered success.
// Two things followed from that: a section the bridge cannot start with (parity
// "Z", tcp_port 0, an unparseable whitelist entry) was written to the operator's
// config file and reported as saved, and a section that *could* start was applied
// to nothing while the bridge was running, because it holds the device it opened
// and the port it bound. These tests pin both: an unrunnable section is refused
// before the file is touched, and a saved one reaches the live service.

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"edgelite/internal/config"
	"edgelite/internal/engine"
)

// testSerialDevice stands in for hardware this host does not have. It records the
// settings the bridge was started with, which is what the apply path has to get
// right, and can refuse a named port so the rollback path is reachable.
type testSerialDevice struct {
	mu     sync.Mutex
	last   engine.SerialBridgeConfig
	failOn string
	device net.Conn
}

func (d *testSerialDevice) open(cfg engine.SerialBridgeConfig) (io.ReadWriteCloser, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failOn != "" && cfg.SerialPort == d.failOn {
		return nil, errors.New("the device disappeared")
	}
	d.last = cfg
	bridgeEnd, deviceEnd := net.Pipe()
	d.device = deviceEnd
	return bridgeEnd, nil
}

func (d *testSerialDevice) lastConfig() engine.SerialBridgeConfig {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.last
}

// withFakeSerialBridge installs the in-memory device for one test and closes it
// afterwards.
func withFakeSerialBridge(t *testing.T) *testSerialDevice {
	t.Helper()
	dev := &testSerialDevice{}
	prevFactory := serialBridgeFactory
	serialBridgeFactory = func() *engine.SerialTCPBridge {
		return engine.NewSerialTCPBridgeWithOpener(dev.open)
	}
	t.Cleanup(func() {
		serialBridgeFactory = prevFactory
		dev.mu.Lock()
		if dev.device != nil {
			_ = dev.device.Close()
		}
		dev.mu.Unlock()
	})
	return dev
}

// freeTCPPort returns a port nobody is holding. The bridge binds the port the
// config names, so a test cannot use ":0" and has to pick one.
func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("release the reserved port: %v", err)
	}
	return port
}

func putSerialBridgeConfig(t *testing.T, body string) (int, string) {
	t.Helper()
	c, rec := setupWithAdmin(echo.PUT, "/api/v1/serial-bridge/config", body)
	if err := handleUpdateSerialBridgeConfig(c); err != nil {
		t.Fatalf("handler returned an error instead of a response: %v", err)
	}
	return rec.Code, rec.Body.String()
}

func TestSerialBridgeConfigRefusesASectionTheBridgeCannotRunWith(t *testing.T) {
	withIsolatedConfig(t)
	cfg := config.GetConfig()
	baseline := config.SerialBridgeConfig{
		Enabled: false, SerialPort: "COM1", BaudRate: 9600, DataBits: 8,
		Parity: "N", StopBits: 1, TCPPort: 19111, MaxClients: 5, IPWhitelist: []string{},
	}
	cfg.SerialBridge = baseline
	// Seed the file with the baseline, or "the file still says baseline" would
	// pass for the wrong reason.
	if err := config.SaveConfig(cfg, ""); err != nil {
		t.Fatalf("seed baseline config: %v", err)
	}

	bodies := map[string]string{
		"no device":             `{"serial_port":"","baud_rate":9600,"tcp_port":19111}`,
		"baud 0":                `{"serial_port":"COM1","baud_rate":0,"tcp_port":19111}`,
		"unknown parity":        `{"serial_port":"COM1","baud_rate":9600,"parity":"Z","tcp_port":19111}`,
		"stop bits 3":           `{"serial_port":"COM1","baud_rate":9600,"stop_bits":3,"tcp_port":19111}`,
		"tcp port out of range": `{"serial_port":"COM1","baud_rate":9600,"tcp_port":70000}`,
		"tcp port 0":            `{"serial_port":"COM1","baud_rate":9600,"tcp_port":0}`,
		"bad whitelist":         `{"serial_port":"COM1","baud_rate":9600,"tcp_port":19111,"ip_whitelist":["not-an-ip"]}`,
		"negative max":          `{"serial_port":"COM1","baud_rate":9600,"tcp_port":19111,"max_clients":-1}`,
	}
	for name, body := range bodies {
		code, resp := putSerialBridgeConfig(t, body)
		if code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400: %s", name, code, resp)
			continue
		}
		var env struct {
			ErrorCode string `json:"error_code"`
			Message   string `json:"message"`
		}
		if err := json.Unmarshal([]byte(resp), &env); err != nil {
			t.Fatalf("%s: unparseable refusal %s: %v", name, resp, err)
		}
		if !strings.Contains(env.ErrorCode, "ERR_SVC_") {
			t.Errorf("%s: refusal carries no service error code the page can map: %s", name, resp)
		}
		if !strings.Contains(env.Message, "serial_bridge") {
			t.Errorf("%s: refusal does not name the setting to fix: %s", name, env.Message)
		}
		// Rejection has to mean nothing was written: a file that carries the
		// rejected section would apply it on the next start.
		saved := persistedConfig(t)
		if serialBridgeSectionsDiffer(saved.SerialBridge, baseline) {
			t.Errorf("%s: rejected body reached the config file: %+v", name, saved.SerialBridge)
		}
	}
}

// serialBridgeSectionsDiffer compares two sections field by field, because the
// file and the bound body disagree about an empty list versus no list at all.
func serialBridgeSectionsDiffer(a, b config.SerialBridgeConfig) bool {
	if a.Enabled != b.Enabled || a.SerialPort != b.SerialPort || a.BaudRate != b.BaudRate ||
		a.DataBits != b.DataBits || a.Parity != b.Parity || a.StopBits != b.StopBits ||
		a.TCPPort != b.TCPPort || a.MaxClients != b.MaxClients || len(a.IPWhitelist) != len(b.IPWhitelist) {
		return true
	}
	for i := range a.IPWhitelist {
		if a.IPWhitelist[i] != b.IPWhitelist[i] {
			return true
		}
	}
	return false
}

func TestSerialBridgeConfigAppliesToTheRunningBridge(t *testing.T) {
	dev := withFakeSerialBridge(t)
	withIsolatedConfig(t)
	first, second := freeTCPPort(t), freeTCPPort(t)
	cfg := config.GetConfig()
	cfg.SerialBridge = config.SerialBridgeConfig{
		Enabled: true, SerialPort: "COM1", BaudRate: 9600, DataBits: 8,
		Parity: "N", StopBits: 1, TCPPort: first, MaxClients: 5,
	}

	cont := GetContainer()
	if err := startEmbeddedSerialBridge(cont); err != nil {
		t.Fatalf("start the bridge under test: %v", err)
	}
	if !serialBridgeIsRunning(cont) {
		t.Fatal("the bridge under test is not running, so the apply path would not be exercised")
	}

	// What the page posts after the operator changes the device, the framing and
	// the TCP port. "enabled" is deliberately wrong: it belongs to the service
	// toggle, and a stale form must not switch the service off in the file.
	body := `{"enabled":false,"serial_port":"COM5","baud_rate":115200,"data_bits":7,"parity":"E","stop_bits":2,"tcp_port":` +
		strconv.Itoa(second) + `,"max_clients":3,"ip_whitelist":["10.0.0.0/8"]}`
	code, resp := putSerialBridgeConfig(t, body)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", code, resp)
	}

	applied := dev.lastConfig()
	if applied.SerialPort != "COM5" || applied.BaudRate != 115200 || applied.DataBits != 7 ||
		applied.Parity != "E" || applied.StopBits != 2 || applied.MaxClients != 3 {
		t.Errorf("the running bridge kept its old device settings, got %+v", applied)
	}
	if applied.ListenAddr != ":"+strconv.Itoa(second) {
		t.Errorf("bridge listens on %q, want the saved port :%d", applied.ListenAddr, second)
	}
	st := cont.SerialBridge.GetStatus()
	if started, _ := st["started"].(bool); !started {
		t.Fatalf("no bridge is running after a successful save: %v", st)
	}
	if err := expectRefused("tcp", "127.0.0.1:"+strconv.Itoa(first), 2*time.Second); err != nil {
		t.Errorf("the old TCP port is still serving clients: %v", err)
	}

	saved := persistedConfig(t)
	if saved.SerialBridge.TCPPort != second || saved.SerialBridge.SerialPort != "COM5" {
		t.Fatalf("the applied settings were not persisted: %+v", saved.SerialBridge)
	}
	if !saved.SerialBridge.Enabled {
		t.Errorf("enabled = false from a stale form switched the service off in the file")
	}
	if saved.SerialBridge.Parity != "E" || saved.SerialBridge.StopBits != 2 {
		t.Errorf("line settings lost on the round trip: %+v", saved.SerialBridge)
	}
}

func TestSerialBridgeConfigRollsBackWhenTheNewSectionCannotRun(t *testing.T) {
	dev := withFakeSerialBridge(t)
	withIsolatedConfig(t)
	first, second := freeTCPPort(t), freeTCPPort(t)
	cfg := config.GetConfig()
	cfg.SerialBridge = config.SerialBridgeConfig{
		Enabled: true, SerialPort: "COM1", BaudRate: 9600, DataBits: 8,
		Parity: "N", StopBits: 1, TCPPort: first, MaxClients: 5,
	}
	cont := GetContainer()
	if err := startEmbeddedSerialBridge(cont); err != nil {
		t.Fatalf("start the bridge under test: %v", err)
	}

	// The section is one Start would accept, but the device is gone: the bridge
	// has to come back on the settings it was running with, and the file has to
	// say the same, or the console shows a state nothing is in.
	dev.mu.Lock()
	dev.failOn = "COM9"
	dev.mu.Unlock()
	body := `{"enabled":true,"serial_port":"COM9","baud_rate":9600,"data_bits":8,"parity":"N","stop_bits":1,"tcp_port":` +
		strconv.Itoa(second) + `,"max_clients":5}`
	code, resp := putSerialBridgeConfig(t, body)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d for a section that could not be applied, want 400: %s", code, resp)
	}
	if !strings.Contains(resp, "ERR_SVC_SERIAL_BRIDGE_RESTART_FAILED") {
		t.Errorf("refusal does not say the restart failed: %s", resp)
	}
	if !serialBridgeIsRunning(cont) {
		t.Fatal("the running bridge was left stopped after a failed apply")
	}
	if got := cont.SerialBridge.GetStatus()["listen_addr"]; got != ":"+strconv.Itoa(first) {
		t.Errorf("after the rollback the bridge serves %v, want the original :%d", got, first)
	}
	saved := persistedConfig(t)
	if saved.SerialBridge.SerialPort != "COM1" || saved.SerialBridge.TCPPort != first {
		t.Errorf("config file kept the section that failed to apply: %+v", saved.SerialBridge)
	}
}

func TestGetSerialBridgeConfigReturnsEveryEditableField(t *testing.T) {
	withIsolatedConfig(t)
	cfg := config.GetConfig()
	cfg.SerialBridge = config.SerialBridgeConfig{
		Enabled: true, SerialPort: "COM3", BaudRate: 19200, DataBits: 7,
		Parity: "O", StopBits: 2, TCPPort: 19123, MaxClients: 0,
		IPWhitelist: []string{"192.168.7.0/24"},
	}

	c, rec := setupWithAdmin(echo.GET, "/api/v1/serial-bridge/config", "")
	if err := handleGetSerialBridgeConfig(c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unparseable response %s: %v", rec.Body, err)
	}
	data, _ := got["data"].(map[string]interface{})
	// The form seeds itself from this answer, so a field the bridge reads has to
	// appear in it - otherwise the page posts back a value it never saw.
	for _, key := range []string{"serial_port", "baud_rate", "data_bits", "parity", "stop_bits", "tcp_port", "max_clients", "ip_whitelist"} {
		if _, ok := data[key]; !ok {
			t.Errorf("GET /serial-bridge/config omits %q: %#v", key, data)
		}
	}
	if data["data_bits"] != float64(7) || data["parity"] != "O" || data["stop_bits"] != float64(2) {
		t.Errorf("line settings came back wrong: %#v", data)
	}
	if wl, ok := data["ip_whitelist"].([]interface{}); !ok || len(wl) != 1 || wl[0] != "192.168.7.0/24" {
		t.Errorf("ip_whitelist came back as %#v", data["ip_whitelist"])
	}
}

// expectRefused waits for a TCP port to stop accepting connections. The bridge
// closes its listener asynchronously, so a dial immediately after the save can
// otherwise succeed against the kernel backlog.
func expectRefused(network, addr string, within time.Duration) error {
	deadline := time.Now().Add(within)
	for {
		conn, err := net.DialTimeout(network, addr, 200*time.Millisecond)
		if err != nil {
			return nil
		}
		_ = conn.Close()
		if time.Now().After(deadline) {
			return errors.New(addr + " is still accepting connections")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
