package drivers

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// A programmable Modbus TCP peer
// ---------------------------------------------------------------------------

// modbusPeerHandler decides what a fake peer answers to one request. The unit ID is
// handed over so a test can model a serial bridge where only some slaves live behind
// the port, and a nil reply keeps the peer silent.
type modbusPeerHandler func(unit byte, pdu []byte) []byte

type modbusRequest struct {
	unit byte
	pdu  []byte
}

// modbusPeer listens on a loopback port and serves Modbus TCP frames with the
// handler under test, recording every request it received.
type modbusPeer struct {
	ln   net.Listener
	host string
	port int
	// unitOverride answers with a unit ID other than the one asked for, which is how
	// a peer that is not the probed device shows up on the wire. 0 echoes the
	// request. It is fixed before the serving goroutine starts, so it is not raced.
	unitOverride byte
	mu           sync.Mutex
	requests     []modbusRequest
	wg           sync.WaitGroup
}

func startModbusPeer(t *testing.T, name string, handle modbusPeerHandler) *modbusPeer {
	t.Helper()
	return startModbusPeerAnsweringUnit(t, name, handle, 0)
}

func startModbusPeerAnsweringUnit(t *testing.T, name string, handle modbusPeerHandler, unitOverride byte) *modbusPeer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen (%s): %v", name, err)
	}
	host, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		_ = ln.Close()
		t.Fatalf("split addr: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		_ = ln.Close()
		t.Fatalf("parse port: %v", err)
	}
	p := &modbusPeer{ln: ln, host: host, port: port, unitOverride: unitOverride}
	p.wg.Add(1)
	go p.serve(handle)
	t.Cleanup(p.close)
	return p
}

func (p *modbusPeer) serve(handle modbusPeerHandler) {
	defer p.wg.Done()
	for {
		conn, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.wg.Add(1)
		go func() {
			defer func() {
				_ = conn.Close()
				p.wg.Done()
			}()
			for {
				header := make([]byte, 7)
				if _, err := io.ReadFull(conn, header); err != nil {
					return
				}
				length := binary.BigEndian.Uint16(header[4:6])
				if length < 2 || length > 254 {
					return
				}
				pdu := make([]byte, int(length)-1)
				if _, err := io.ReadFull(conn, pdu); err != nil {
					return
				}
				p.mu.Lock()
				p.requests = append(p.requests, modbusRequest{unit: header[6], pdu: pdu})
				p.mu.Unlock()

				reply := handle(header[6], pdu)
				if reply == nil {
					continue
				}
				answerUnit := header[6]
				if p.unitOverride != 0 {
					answerUnit = p.unitOverride
				}
				frame := make([]byte, 7+len(reply))
				copy(frame[0:2], header[0:2])
				binary.BigEndian.PutUint16(frame[4:6], uint16(len(reply)+1))
				frame[6] = answerUnit
				copy(frame[7:], reply)
				if _, err := conn.Write(frame); err != nil {
					return
				}
			}
		}()
	}
}

func (p *modbusPeer) close() {
	_ = p.ln.Close()
	p.wg.Wait()
}

// seen returns the unit IDs the peer was asked about, in arrival order.
func (p *modbusPeer) seen() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	units := make([]byte, 0, len(p.requests))
	for _, r := range p.requests {
		units = append(units, r.unit)
	}
	return units
}

// identifiedSlave answers Read Device Information the way a Modbus TCP gateway must,
// and answers a coil read normally.
func identifiedSlave(unit byte, pdu []byte) []byte {
	if len(pdu) == 0 {
		return nil
	}
	switch pdu[0] {
	case 0x2B:
		return []byte{0x2B, 0x0E, 0x01, 0x01, 0x00, 0x01, 0x2B, 0x00, 0x01, 0x01, 'P'}
	case 0x01:
		return []byte{0x01, 0x01, 0x00}
	default:
		return []byte{pdu[0] | 0x80, 0x01}
	}
}

// bridgeSlave models a serial-to-TCP bridge: MEI 0x0E is unknown, but the coil
// table of the slave behind the port answers.
func bridgeSlave(unit byte, pdu []byte) []byte {
	if len(pdu) == 0 {
		return nil
	}
	switch pdu[0] {
	case 0x2B:
		return []byte{0xAB, 0x01}
	case 0x01:
		return []byte{0x01, 0x01, 0x01}
	default:
		return []byte{pdu[0] | 0x80, 0x01}
	}
}

// exceptionSlave speaks Modbus framing but exposes nothing at the probed address.
func exceptionSlave(unit byte, pdu []byte) []byte {
	if len(pdu) == 0 {
		return nil
	}
	if pdu[0] == 0x2B {
		return []byte{0xAB, 0x0A}
	}
	return []byte{pdu[0] | 0x80, 0x02}
}

// silentSlave accepts the connection and never answers.
func silentSlave(unit byte, pdu []byte) []byte { return nil }

// onlyUnit answers for one unit ID and withholds every other unit's device.
func onlyUnit(want byte, inner modbusPeerHandler) modbusPeerHandler {
	return func(unit byte, pdu []byte) []byte {
		if unit != want {
			return nil
		}
		return inner(unit, pdu)
	}
}

// probeTarget points a probe at a listening fake peer.
func (p *modbusPeer) target(unit int) modbusTarget {
	return modbusTarget{ip: p.host, port: p.port, unit: unit}
}

func freeTarget() modbusTarget {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	_ = ln.Close()
	return modbusTarget{ip: "127.0.0.1", port: port, unit: 1}
}

// ---------------------------------------------------------------------------
// probeModbusUnit
// ---------------------------------------------------------------------------

func TestProbeModbusUnitAcceptsIdentifiedSlave(t *testing.T) {
	p := startModbusPeer(t, "identified", identifiedSlave)
	if !probeModbusUnit(context.Background(), p.target(1)) {
		t.Fatal("a slave that identified itself over MEI 0x0E was not reported")
	}
	if got := p.seen(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("requests = %v, want exactly one for unit 1", got)
	}
}

func TestProbeModbusUnitAcceptsCoilsWhenDeviceIDUnsupported(t *testing.T) {
	p := startModbusPeer(t, "bridge", bridgeSlave)
	if !probeModbusUnit(context.Background(), p.target(3)) {
		t.Fatal("a bridge that refuses Read Device Information but answers coil 0 was not reported")
	}
	if got := p.seen(); len(got) != 2 {
		t.Fatalf("requests = %d, want the device-ID probe followed by the coil probe", len(got))
	}
}

func TestProbeModbusUnitRejectsExceptionOnlyPeer(t *testing.T) {
	// The rule this locks down: an exception is ambiguous ("no such unit" or "no
	// such address"), so the scanner stays quiet instead of listing 247 ghosts.
	p := startModbusPeer(t, "exceptions", exceptionSlave)
	if probeModbusUnit(context.Background(), p.target(1)) {
		t.Fatal("a peer that refused both probes was listed as a device")
	}
}

func TestProbeModbusUnitRejectsSilentPort(t *testing.T) {
	p := startModbusPeer(t, "silent", silentSlave)
	start := time.Now()
	if probeModbusUnit(context.Background(), p.target(1)) {
		t.Fatal("an open port that never answered was listed as a device")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the silent probe took %v; it must give up on its own deadlines", elapsed)
	}
}

func TestProbeModbusUnitRejectsWrongUnitEcho(t *testing.T) {
	// A reply that names a different unit than asked is not proof of this unit: it
	// is what a proxy answering on behalf of the whole port looks like.
	p := startModbusPeerAnsweringUnit(t, "wrong-unit", identifiedSlave, 42)
	if probeModbusUnit(context.Background(), p.target(1)) {
		t.Fatal("a reply carrying another unit ID was accepted")
	}
}

func TestProbeModbusUnitRejectsClosedPort(t *testing.T) {
	if probeModbusUnit(context.Background(), freeTarget()) {
		t.Fatal("a closed port was listed as a device")
	}
}

func TestProbeModbusUnitRejectsNonModbusPeer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		// An HTTP error page: a port that answers is not a Modbus slave.
		_, _ = conn.Write([]byte("HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n"))
		_ = conn.Close()
	}()
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	if probeModbusUnit(context.Background(), modbusTarget{ip: "127.0.0.1", port: port, unit: 1}) {
		t.Fatal("a non-Modbus service was listed as a device")
	}
}

func TestProbeModbusUnitHonoursContextCancellation(t *testing.T) {
	p := startModbusPeer(t, "silent-cancel", silentSlave)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if probeModbusUnit(ctx, p.target(1)) {
		t.Fatal("a cancelled context must not report a device")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("cancelled probe took %v", elapsed)
	}
}

// ---------------------------------------------------------------------------
// ModbusTCPDriver.Discover
// ---------------------------------------------------------------------------

func TestModbusTCPDiscoverReportsIdentifiedSlave(t *testing.T) {
	p := startModbusPeer(t, "discover", onlyUnit(7, identifiedSlave))
	driver, err := NewModbusTCPDriver("discover-test", nil)
	if err != nil {
		t.Fatalf("create driver: %v", err)
	}

	devices, err := driver.Discover(context.Background(), map[string]interface{}{
		"host":       p.host,
		"port":       p.port,
		"slave_id":   7,
		"scan_units": 8,
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(devices) != 1 {
		t.Fatalf("devices = %d (%v), want only the unit that answered", len(devices), devices)
	}
	got := devices[0]
	if got["ip"] != p.host || got["port"] != p.port {
		t.Errorf("entry address = %v:%v, want %s:%d", got["ip"], got["port"], p.host, p.port)
	}
	if got["unit_id"] != 7 || got["slave_id"] != 7 {
		t.Errorf("entry unit = %v/%v, want 7/7", got["unit_id"], got["slave_id"])
	}
	if got["protocol"] != "modbus_tcp" {
		t.Errorf("entry protocol = %v", got["protocol"])
	}
	if got["device_id"] != fmt.Sprintf("modbus-%s-%d-7", p.host, p.port) {
		t.Errorf("entry device_id = %v", got["device_id"])
	}
}

func TestModbusTCPDiscoverProbesOnlyConfiguredUnitByDefault(t *testing.T) {
	p := startModbusPeer(t, "default-units", identifiedSlave)
	driver, _ := NewModbusTCPDriver("discover-default", nil)
	if _, err := driver.Discover(context.Background(), map[string]interface{}{
		"host":     p.host,
		"port":     p.port,
		"slave_id": 3,
	}); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got := p.seen(); len(got) != 1 || got[0] != 3 {
		t.Fatalf("units asked = %v, want only the configured slave_id 3", got)
	}
}

func TestModbusTCPDiscoverRejectsNonModbusPort(t *testing.T) {
	p := startModbusPeer(t, "exceptions-discover", exceptionSlave)
	driver, _ := NewModbusTCPDriver("discover-none", nil)
	devices, err := driver.Discover(context.Background(), map[string]interface{}{
		"host":     p.host,
		"port":     p.port,
		"slave_id": 1,
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(devices) != 0 {
		t.Fatalf("devices = %v, want none from a port that answered only exceptions", devices)
	}
}

func TestModbusUnitsToProbe(t *testing.T) {
	cases := []struct {
		name   string
		config map[string]interface{}
		want   []int
	}{
		{"default asks the configured unit", map[string]interface{}{"slave_id": 9}, []int{9}},
		{"no slave_id defaults to 1", nil, []int{1}},
		{"zero sweep means the configured unit", map[string]interface{}{"scan_units": 0, "slave_id": 2}, []int{2}},
		{"sweep lists 1..N", map[string]interface{}{"scan_units": 4}, []int{1, 2, 3, 4}},
		{"sweep clamps to the addressable range", map[string]interface{}{"scan_units": 5000}, allUnits()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := modbusUnitsToProbe(tc.config)
			if len(got) != len(tc.want) {
				t.Fatalf("units = %d, want %d", len(got), len(tc.want))
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("units = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func allUnits() []int {
	units := make([]int, 0, modbusMaxUnitID)
	for i := 1; i <= modbusMaxUnitID; i++ {
		units = append(units, i)
	}
	return units
}

func TestExpandModbusHostRange(t *testing.T) {
	cases := []struct {
		name    string
		host    string
		count   int
		want    []string
		wantErr string
	}{
		{name: "walks upward", host: "192.168.1.10", count: 2, want: []string{"192.168.1.10", "192.168.1.11", "192.168.1.12"}},
		{name: "clamped to the /24", host: "10.0.0.254", count: 50, want: []string{"10.0.0.254", "10.0.0.255"}},
		{name: "hostname rejected", host: "plc.example.com", count: 1, wantErr: "IPv4"},
		{name: "ipv6 rejected", host: "::1", count: 1, wantErr: "IPv4"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := expandModbusHostRange(tc.host, tc.count)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want one mentioning %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("hosts = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestModbusTCPDiscoverRejectsNonIPHostWithScanRange(t *testing.T) {
	driver, _ := NewModbusTCPDriver("discover-range", nil)
	_, err := driver.Discover(context.Background(), map[string]interface{}{
		"host":       "plc.example.com",
		"port":       502,
		"scan_range": 5,
	})
	if err == nil || !strings.Contains(err.Error(), "IPv4") {
		t.Fatalf("error = %v, want one explaining that scan_range needs an IPv4 host", err)
	}
}

// ---------------------------------------------------------------------------
// Drivers that cannot discover anything
// ---------------------------------------------------------------------------

// TestDriversWithoutDiscoverySaySo covers every protocol whose Discover cannot ask a
// device: an empty list used to be rendered as "scanned, found nothing", which is a
// different claim from "this gateway cannot ask".
func TestDriversWithoutDiscoverySaySo(t *testing.T) {
	config := map[string]interface{}{
		"host":        "127.0.0.1",
		"port":        502,
		"serial_port": "COM1",
		"endpoint":    "opc.tcp://127.0.0.1:4840",
		"broker":      "127.0.0.1",
		"slot":        0,
	}
	protocols := map[string]func(string, map[string]interface{}) (Driver, error){
		"siemens_s7":    NewS7Driver,
		"mitsubishi_mc": NewMCDriver,
		"omron_fins":    NewFINSDriver,
		"allen_bradley": NewABDriver,
		"modbus_rtu":    NewModbusRTUDriver,
		"modbus_slave":  NewModbusSlaveDriver,
		"mqtt_client":   NewMQTTClientDriver,
		"http_webhook":  NewHTTPWebhookDriver,
		"simulator":     NewSimulatorDriver,
	}
	for name, factory := range protocols {
		t.Run(name, func(t *testing.T) {
			driver, err := factory("discover-unsupported-"+name, config)
			if err != nil {
				t.Fatalf("create %s driver: %v", name, err)
			}
			devices, err := driver.Discover(context.Background(), config)
			if !errors.Is(err, ErrDiscoveryUnsupported) {
				t.Fatalf("Discover error = %v, want ErrDiscoveryUnsupported", err)
			}
			if len(devices) != 0 {
				t.Fatalf("devices = %v, want none alongside the error", devices)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// OPC UA discovery
// ---------------------------------------------------------------------------

func TestOPCUADiscoverRequiresAnAddress(t *testing.T) {
	// Nothing to ask and nothing configured: the driver must say so rather than
	// return an empty list that reads as "no server on that address".
	empty := map[string]interface{}{"endpoint": "", "server_url": ""}
	driver, err := NewOPCUADriver("opcua-no-endpoint", empty)
	if err != nil {
		t.Fatalf("create driver: %v", err)
	}
	if _, err := driver.Discover(context.Background(), empty); err == nil {
		t.Fatal("Discover without an endpoint or host must explain itself, not scan nothing")
	}
}

func TestOPCUADiscoverRejectsPortThatIsNotAServer(t *testing.T) {
	// The old implementation listed any port that accepted a TCP connection. A
	// listener that hangs up proves the difference.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = conn.Write([]byte("not opc ua at all"))
			_ = conn.Close()
		}
	}()

	driver, _ := NewOPCUADriver("opcua-not-a-server", nil)
	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	devices, err := driver.Discover(context.Background(), map[string]interface{}{"host": host, "port": port})
	if err != nil {
		t.Fatalf("an endpoint that is not an OPC UA server is a result, not a gateway fault: %v", err)
	}
	if len(devices) != 0 {
		t.Fatalf("devices = %v, want none", devices)
	}
}

// ---------------------------------------------------------------------------
// ONVIF discovery messages
// ---------------------------------------------------------------------------

func TestONVIFProbeDeclaresItsTypeNamespace(t *testing.T) {
	// Without the dn declaration cameras fault the probe and every scan is empty.
	if !strings.Contains(onvifProbeMessage, `xmlns:dn="http://www.onvif.org/ver10/network/wsdl"`) {
		t.Fatal("the WS-Discovery probe uses dn: without declaring the namespace")
	}
	if !strings.Contains(onvifProbeMessage, "dn:NetworkVideoTransmitter") {
		t.Fatal("the probe no longer asks for ONVIF network video transmitters")
	}
}

func TestParseONVIFDeviceAddresses(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want []string
	}{
		{
			name: "single address",
			doc:  `<ProbeMatches><ProbeMatch><XAddrs>http://192.0.2.7/onvif/device_service</XAddrs></ProbeMatch></ProbeMatches>`,
			want: []string{"http://192.0.2.7/onvif/device_service"},
		},
		{
			name: "namespace attributes",
			doc:  `<ProbeMatch><XAddrs xmlns="http://schemas.xmlsoap.org/ws/2005/04/discovery">http://192.0.2.8/onvif</XAddrs></ProbeMatch>`,
			want: []string{"http://192.0.2.8/onvif"},
		},
		{
			name: "several addresses",
			doc:  `<ProbeMatch><XAddrs>http://192.0.2.9/onvif https://192.0.2.9/onvif</XAddrs></ProbeMatch>`,
			want: []string{"http://192.0.2.9/onvif", "https://192.0.2.9/onvif"},
		},
		{name: "no XAddrs", doc: `<Bye/>`, want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseONVIFDeviceAddresses(tc.doc)
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Fatalf("addresses = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestONVIFAddressEntry(t *testing.T) {
	cases := []struct {
		name     string
		xaddr    string
		wantIP   string
		wantPort int
		wantOK   bool
	}{
		{name: "explicit port", xaddr: "http://192.0.2.7:8080/onvif/device_service", wantIP: "192.0.2.7", wantPort: 8080, wantOK: true},
		{name: "http default", xaddr: "http://192.0.2.7/onvif", wantIP: "192.0.2.7", wantPort: 80, wantOK: true},
		{name: "https default", xaddr: "https://192.0.2.7/onvif", wantIP: "192.0.2.7", wantPort: 443, wantOK: true},
		{name: "no host", xaddr: "/onvif/device_service"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry, ok := onvifAddressEntry(tc.xaddr)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if entry["ip"] != tc.wantIP || entry["port"] != tc.wantPort {
				t.Fatalf("entry = %v, want %s:%d", entry, tc.wantIP, tc.wantPort)
			}
			if entry["protocol"] != "onvif" || entry["device_id"] == nil {
				t.Fatalf("entry must name the protocol and carry a device id: %v", entry)
			}
		})
	}
}
