package api

// GET /serial-bridge/ports answered the literal {"COM1","COM2","COM3",
// "/dev/ttyS0","/dev/ttyUSB0"} whatever the host had plugged in. That list mixes
// Windows and Linux names, so it cannot describe one real machine, and a caller
// reading it could not tell a detected device from an invented one. The handler
// now asks the operating system and says so when it cannot.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"edgelite/internal/config"
)

// withSerialPorts points the enumerator and the configured device at test values
// and puts both back afterwards.
func withSerialPorts(t *testing.T, ports []string, err error, configured string) {
	t.Helper()
	prevList := serialPortsEnumerator
	prevConfig := config.GetConfig()
	t.Cleanup(func() {
		serialPortsEnumerator = prevList
		config.SetGlobalConfig(prevConfig)
	})
	serialPortsEnumerator = func() ([]string, error) { return ports, err }
	cfg := &config.AppConfig{}
	cfg.SerialBridge.SerialPort = configured
	config.SetGlobalConfig(cfg)
}

func TestSerialPortsAnswerWithWhatTheHostReports(t *testing.T) {
	withSerialPorts(t, []string{"COM9", "/dev/ttyUSB7"}, nil, "COM9")

	c, rec := setupWithAdmin("GET", "/api/v1/serial-bridge/ports", "")
	if err := handleListSerialPorts(c); err != nil {
		t.Fatalf("handleListSerialPorts: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var env struct {
		Data struct {
			Ports             []string `json:"ports"`
			Total             int      `json:"total"`
			Configured        string   `json:"configured"`
			ConfiguredPresent *bool    `json:"configured_present"`
			Note              string   `json:"note"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unparseable response %s: %v", rec.Body, err)
	}
	got := env.Data.Ports
	if len(got) != 2 || got[0] != "/dev/ttyUSB7" || got[1] != "COM9" {
		t.Fatalf("ports = %#v, want exactly the two the host reported, sorted", got)
	}
	if env.Data.Total != 2 {
		t.Errorf("total = %d for %#v", env.Data.Total, got)
	}
	if env.Data.ConfiguredPresent == nil || !*env.Data.ConfiguredPresent {
		t.Errorf("COM9 was reported by the host, so configured_present must say so: %#v", env.Data)
	}

	// A configured device that is not plugged in stays out of the list: offering
	// it would recreate the old lie in a new place. But it has to be named, or the
	// page cannot explain why its saved value is missing.
	withSerialPorts(t, []string{"COM9"}, nil, "COM42")
	c, rec = setupWithAdmin("GET", "/api/v1/serial-bridge/ports", "")
	if err := handleListSerialPorts(c); err != nil {
		t.Fatalf("handleListSerialPorts with an absent configured port: %v", err)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unparseable response %s: %v", rec.Body, err)
	}
	if len(env.Data.Ports) != 1 || env.Data.Ports[0] != "COM9" {
		t.Errorf("ports = %#v, want the configured-but-absent device left out", env.Data.Ports)
	}
	if env.Data.Configured != "COM42" || env.Data.ConfiguredPresent == nil || *env.Data.ConfiguredPresent {
		t.Errorf("configured = %q/%#v, want COM42 named and marked absent", env.Data.Configured, env.Data.ConfiguredPresent)
	}
}

func TestSerialPortsSayWhenThereIsNothingToAskOrNoAnswer(t *testing.T) {
	// A host with no serial hardware is a real state, and the answer has to read
	// as that rather than as an unexplained empty chooser.
	withSerialPorts(t, []string{}, nil, "")
	c, rec := setupWithAdmin("GET", "/api/v1/serial-bridge/ports", "")
	if err := handleListSerialPorts(c); err != nil {
		t.Fatalf("handleListSerialPorts on an empty host: %v", err)
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			Ports []string `json:"ports"`
			Note  string   `json:"note"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unparseable response %s: %v", rec.Body, err)
	}
	if env.Data.Ports == nil {
		t.Errorf("ports = nil, want an empty list a caller can iterate")
	}
	if env.Data.Note == "" {
		t.Errorf("an empty list arrived with no explanation: %s", rec.Body)
	}

	// When the OS refuses the question the endpoint has to fail rather than fall
	// back to the list it used to hardcode.
	withSerialPorts(t, nil, errors.New("permission denied on /dev/ttyS*"), "")
	c, rec = setupWithAdmin("GET", "/api/v1/serial-bridge/ports", "")
	if err := handleListSerialPorts(c); err != nil {
		t.Fatalf("handleListSerialPorts with a failing enumerator: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d with the OS refusing to answer, want 503: %s", rec.Code, rec.Body)
	}
	var fail struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &fail); err != nil {
		t.Fatalf("unparseable refusal %s: %v", rec.Body, err)
	}
	for _, invented := range []string{"COM1", "COM2", "COM3", "ttyUSB0"} {
		if strings.Contains(fail.Message, invented) {
			t.Errorf("the refusal carries the fabricated device %s: %s", invented, fail.Message)
		}
	}
}
