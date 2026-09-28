package drivers

import "testing"

// TestNormalizeDriverConfig pins the frontend-key -> driver-key reconciliation
// used by CreateDriver. These are cross-layer bugs: the device form in
// frontend/src/constants/protocolConfig.ts stores values under key names that
// differ from what each driver constructor reads, so without reconciliation the
// driver silently falls back to its default and ignores the operator's input.
func TestNormalizeDriverConfig(t *testing.T) {
	// Allen-Bradley: form uses "ip"/"plc_model"; driver reads "host"/"plc_type".
	ab := map[string]interface{}{"ip": "10.1.2.3", "plc_model": "CompactLogix", "port": float64(44818)}
	NormalizeDriverConfig("allen_bradley", ab)
	if ab["host"] != "10.1.2.3" {
		t.Errorf("allen_bradley host = %v, want 10.1.2.3", ab["host"])
	}
	if ab["plc_type"] != "CompactLogix" {
		t.Errorf("allen_bradley plc_type = %v, want CompactLogix", ab["plc_type"])
	}

	// ONVIF: form uses "ip"; driver reads "host".
	ov := map[string]interface{}{"ip": "192.168.1.50"}
	NormalizeDriverConfig("onvif", ov)
	if ov["host"] != "192.168.1.50" {
		t.Errorf("onvif host = %v, want 192.168.1.50", ov["host"])
	}

	// FINS: form uses "dest_node"/"unit_no"; driver reads "node"/"unit".
	fins := map[string]interface{}{"dest_node": float64(12), "unit_no": float64(0)}
	NormalizeDriverConfig("omron_fins", fins)
	if fins["node"] != float64(12) {
		t.Errorf("fins node = %v, want 12", fins["node"])
	}
	// unit_no==0 must still bridge: numeric zero is a valid unit, not "empty".
	if _, ok := fins["unit"]; !ok {
		t.Errorf("fins unit missing: numeric 0 must not be treated as empty")
	}

	// Modbus RTU: the serial path is stored under "port" (string); the driver
	// wants "serial_port" for the path and "port" (int) for the TCP bridge.
	rtu := map[string]interface{}{"port": "COM3", "baudrate": float64(19200), "parity": "E", "unit_id": float64(7)}
	NormalizeDriverConfig("modbus_rtu", rtu)
	if rtu["serial_port"] != "COM3" {
		t.Errorf("rtu serial_port = %v, want COM3", rtu["serial_port"])
	}
	if rtu["baud_rate"] != float64(19200) {
		t.Errorf("rtu baud_rate = %v, want 19200", rtu["baud_rate"])
	}
	if rtu["slave_id"] != float64(7) {
		t.Errorf("rtu slave_id = %v, want 7", rtu["slave_id"])
	}
	if rtu["parity"] != "even" {
		t.Errorf("rtu parity = %v, want even (letter E normalized)", rtu["parity"])
	}

	// Fill-on-missing: a driver-native key already set is never overwritten by an
	// alias, so hand-authored configs keep working.
	keep := map[string]interface{}{"host": "1.1.1.1", "ip": "2.2.2.2"}
	NormalizeDriverConfig("allen_bradley", keep)
	if keep["host"] != "1.1.1.1" {
		t.Errorf("existing host overwritten: got %v, want 1.1.1.1", keep["host"])
	}

	// A numeric "port" for RTU is a TCP bridge port and must NOT be bridged into
	// the string serial_port.
	numeric := map[string]interface{}{"port": float64(5020)}
	NormalizeDriverConfig("modbus_rtu", numeric)
	if _, ok := numeric["serial_port"]; ok {
		t.Errorf("numeric port leaked into serial_port: %v", numeric["serial_port"])
	}
}

// TestNormalizeDriverConfigViaCreateDriver proves the wiring: constructing an
// Allen-Bradley driver through the registry with UI-style config yields a driver
// whose effective host is the value the operator typed under "ip", not the
// 127.0.0.1 default.
func TestNormalizeDriverConfigViaCreateDriver(t *testing.T) {
	RegisterAll()
	d, err := GetRegistry().CreateDriver("allen_bradley", "ab-ui", map[string]interface{}{
		"ip": "172.16.0.9", "port": float64(44818),
	})
	if err != nil {
		t.Fatalf("CreateDriver: %v", err)
	}
	ab, ok := d.(*ABDriver)
	if !ok {
		t.Fatalf("expected *ABDriver, got %T", d)
	}
	if ab.host != "172.16.0.9" {
		t.Fatalf("ABDriver.host = %q, want 172.16.0.9 (UI 'ip' must reach driver 'host')", ab.host)
	}
	if ab.port != 44818 {
		t.Fatalf("ABDriver.port = %d, want 44818", ab.port)
	}
}

// TestCreateDriverDoesNotMutateCallerConfig guards the copy-on-normalize: the
// driver's effective host comes from the "ip" alias, but the caller's map (the
// live device.Config) must not gain a "host" key behind other readers' backs.
func TestCreateDriverDoesNotMutateCallerConfig(t *testing.T) {
	RegisterAll()
	cfg := map[string]interface{}{"ip": "10.9.8.7"}
	if _, err := GetRegistry().CreateDriver("allen_bradley", "ab-copy", cfg); err != nil {
		t.Fatalf("CreateDriver: %v", err)
	}
	if _, ok := cfg["host"]; ok {
		t.Fatalf("caller config mutated: CreateDriver injected \"host\"=%v into the caller's map", cfg["host"])
	}
}
