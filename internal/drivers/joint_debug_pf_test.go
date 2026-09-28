package drivers

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"net"
	"strings"
	"testing"
	"time"

	"edgelite/internal/models"
)

// Deep ProtoForge joint-debugging added on top of the per-protocol "contract"
// and "live" tests. Two concerns this file locks in, which the earlier tests
// did not:
//
//   1. WRITE -> READ-BACK round trips for the protocols that previously only
//      had read-quality smoke coverage (Modbus TCP, Mitsubishi MC). S7 and
//      Omron FINS already assert round-trip values in their own *_pf_test.go
//      files. These use unowned high addresses because the ProtoForge
//      simulators persist writes to unowned ranges indefinitely, whereas
//      point-backed addresses are re-synced by the simulator tick.
//
//   2. VALUE HONESTY: a driver must never report Quality:"good" for a value it
//      did not actually read. A malformed/unparseable address, and a server
//      that lies about a response byte-count, must degrade to Quality:"bad"
//      with a nil value — and must never panic. The ProtoForge S7/MC/Modbus
//      servers answer unknown addresses with "success + zeros", so a real
//      fabrication can only be provoked here by driving the driver with bad
//      input directly (malformed address) or a deliberately short response
//      from a hermetic mock, not by pointing it at ProtoForge.

// pfTCPUp reports whether a server is listening on addr, for skip guards.
//
// NOTE: a bare TCP dial is only a reliable "is this ProtoForge?" signal for the
// protocol ports EdgeLite's own gateway never binds — OPC-UA 4840, AB 44818,
// S7 102, MC 5000, FINS 9600, MQTT 1883. It is NOT reliable for Modbus (5020)
// or the HTTP webhook target (8080), because the gateway's own Modbus slave
// (configs modbus_slave.port=5020) and Echo API (server.port=8080) squat those
// exact ports. A test keyed only on 5020/8080 being open can therefore "pass"
// against the gateway itself while ProtoForge is entirely absent — a false
// joint-debug verification. Those tests must additionally require pfAPIUp().
func pfTCPUp(t *testing.T, addr string) bool {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// pfAPIUp reports whether ProtoForge's management REST API is listening on its
// host port 8000. EdgeLite's gateway never binds 8000 in any supported config
// (its own API is 8080, remapped to 8081 under the joint-debug compose), so a
// live 8000 is an unambiguous fingerprint that a real ProtoForge is running.
// Guards for the 5020/8080 tests AND this together to remove the ambiguity.
func pfAPIUp(t *testing.T) bool {
	t.Helper()
	return pfTCPUp(t, "127.0.0.1:8000")
}

// pfLive is the correct skip guard for a ProtoForge protocol server: it requires
// the unambiguous PF-8000 API fingerprint and then the specific simulator port.
func pfLive(t *testing.T, simAddr string) {
	t.Helper()
	if !pfAPIUp(t) {
		t.Skip("ProtoForge management API not on 127.0.0.1:8000 — PF not running; refusing to test a port the gateway may be squatting")
	}
	if !pfTCPUp(t, simAddr) {
		t.Skipf("ProtoForge simulator not reachable on %s", simAddr)
	}
}

// ---------------------------------------------------------------------------
// Modbus TCP: write -> read-back (live ProtoForge, unowned holding registers)
// ---------------------------------------------------------------------------

func TestJointDebug_ModbusWriteReadBackLive(t *testing.T) {
	pfLive(t, "127.0.0.1:5020")
	d, err := NewModbusTCPDriver("jd-modbus", map[string]interface{}{
		"host": "127.0.0.1", "port": 5020, "slave_id": 1, "timeout": 3,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := d.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer d.Disconnect()

	// Single holding register round trip at an offset the simulator never owns.
	const hr = "HR620"
	if err := d.WritePoint(ctx, hr, uint16(31337)); err != nil {
		t.Fatalf("WritePoint(%s): %v", hr, err)
	}
	rows, err := d.(*ModbusTCPDriver).ReadPoints(ctx, []models.PointDef{
		{Name: "r", Address: hr, DataType: "uint16"},
	})
	if err != nil {
		t.Fatalf("ReadPoints: %v", err)
	}
	if len(rows) != 1 || rows[0].Quality != "good" {
		t.Fatalf("read-back quality = %+v, want good", rows[0])
	}
	if got, ok := rows[0].Value.(uint16); !ok || got != 31337 {
		t.Fatalf("read-back %v (%T), want uint16 31337", rows[0].Value, rows[0].Value)
	}

	// A second, different value must overwrite the first (proves the write
	// reached persistent memory rather than a read of a stale default).
	if err := d.WritePoint(ctx, hr, uint16(7)); err != nil {
		t.Fatalf("WritePoint overwrite: %v", err)
	}
	rows, err = d.(*ModbusTCPDriver).ReadPoints(ctx, []models.PointDef{
		{Name: "r", Address: hr, DataType: "uint16"},
	})
	if err != nil {
		t.Fatalf("ReadPoints after overwrite: %v", err)
	}
	if got, ok := rows[0].Value.(uint16); !ok || got != 7 {
		t.Fatalf("overwrite read-back %v (%T), want uint16 7", rows[0].Value, rows[0].Value)
	}
}

// ---------------------------------------------------------------------------
// Mitsubishi MC: write -> read-back (live ProtoForge, unowned D words)
// ---------------------------------------------------------------------------

func TestJointDebug_MCWriteReadBackLive(t *testing.T) {
	if !pfTCPUp(t, "127.0.0.1:5000") {
		t.Skip("ProtoForge Mitsubishi MC simulator not reachable on 127.0.0.1:5000")
	}
	RegisterAll()
	d, err := GetRegistry().CreateDriver("mitsubishi_mc", "jd-mc", map[string]interface{}{
		"host": "127.0.0.1", "port": 5000, "plc_type": "Q", "timeout": 3,
	})
	if err != nil {
		t.Fatalf("CreateDriver: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := d.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer d.Disconnect()

	// Integer word round trip (D300 is within the simulator's 1024-byte D area
	// and is not owned by any served point).
	if err := d.WritePoint(ctx, "D300", 4242); err != nil {
		t.Fatalf("WritePoint(D300): %v", err)
	}
	rows, err := d.ReadPoints(ctx, []models.PointDef{{Name: "w", Address: "D300", DataType: "uint16"}})
	if err != nil {
		t.Fatalf("ReadPoints D300: %v", err)
	}
	if len(rows) != 1 || rows[0].Quality != "good" {
		t.Fatalf("D300 read-back quality = %+v, want good", rows[0])
	}
	if got, ok := rows[0].Value.(uint16); !ok || got != 4242 {
		t.Fatalf("D300 read-back %v (%T), want uint16 4242", rows[0].Value, rows[0].Value)
	}

	// REAL (float32) round trip over two words.
	if err := d.WritePoint(ctx, "D304", float32(-3.5)); err != nil {
		t.Fatalf("WritePoint(D304): %v", err)
	}
	rows, err = d.ReadPoints(ctx, []models.PointDef{{Name: "f", Address: "D304", DataType: "float32"}})
	if err != nil {
		t.Fatalf("ReadPoints D304: %v", err)
	}
	if len(rows) != 1 || rows[0].Quality != "good" {
		t.Fatalf("D304 read-back quality = %+v, want good", rows[0])
	}
	if got, ok := rows[0].Value.(float32); !ok || math.Abs(float64(got)-(-3.5)) > 1e-4 {
		t.Fatalf("D304 read-back %v (%T), want float32 -3.5", rows[0].Value, rows[0].Value)
	}
}

// ---------------------------------------------------------------------------
// Honesty: malformed addresses must yield Quality:"bad", never a fabricated
// "good", and must never panic. These exercise the driver's own input path so
// they do not depend on the simulator's (zero-fabricating) behavior.
// ---------------------------------------------------------------------------

func TestJointDebug_MalformedAddressHonesty(t *testing.T) {
	cases := []struct {
		name     string
		protocol string
		config   map[string]interface{}
		port     string
		address  string
		dataT    string
	}{
		{"modbus", "modbus_tcp", map[string]interface{}{"host": "127.0.0.1", "port": 5020, "slave_id": 1, "timeout": 3}, "127.0.0.1:5020", "NOT_A_REAL_ADDRESS", "uint16"},
		{"modbus-empty", "modbus_tcp", map[string]interface{}{"host": "127.0.0.1", "port": 5020, "slave_id": 1, "timeout": 3}, "127.0.0.1:5020", "HR", "uint16"},
		{"s7", "siemens_s7", map[string]interface{}{"ip": "127.0.0.1", "port": 102, "rack": 0, "slot": 1, "timeout": 3}, "127.0.0.1:102", "@@garbage@@", "int16"},
	}
	RegisterAll()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !pfTCPUp(t, c.port) {
				t.Skipf("ProtoForge %s simulator not reachable on %s", c.protocol, c.port)
			}
			d, err := GetRegistry().CreateDriver(c.protocol, "jd-honest-"+c.name, c.config)
			if err != nil {
				t.Fatalf("CreateDriver: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			defer cancel()
			if err := d.Connect(ctx); err != nil {
				t.Fatalf("connect: %v", err)
			}
			defer d.Disconnect()

			got, err := d.ReadPoints(ctx, []models.PointDef{{Name: "x", Address: c.address, DataType: c.dataT}})
			if err != nil {
				// A hard error instead of a per-point bad row is also honest.
				return
			}
			if len(got) != 1 {
				t.Fatalf("want 1 row, got %d", len(got))
			}
			if got[0].Quality == "good" {
				t.Fatalf("malformed address %q reported Quality=good value=%v — fabricated reading", c.address, got[0].Value)
			}
			if got[0].Value != nil {
				t.Fatalf("malformed address %q returned non-nil value %v on bad quality", c.address, got[0].Value)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Regression for the response length-guard panic fix: a Modbus server that
// advertises a byte count larger than the payload it actually sends used to
// drive an index-out-of-range panic in readInputRegisters/readCoils/
// readDiscreteInputs. Those now return an error -> Quality:"bad".
// ---------------------------------------------------------------------------

// serveLyingModbusInput accepts one request and replies with a FC04 response
// whose declared byte count (200) exceeds the bytes actually transmitted, then
// closes. This is what a buggy/hostile server does.
func serveLyingModbusInput(t *testing.T, ln net.Listener) {
	t.Helper()
	conn, err := ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	// Drain the request: MBAP header is 7 bytes, then (length-1) PDU bytes.
	hdr := make([]byte, 7)
	if _, err := readFull(conn, hdr); err != nil {
		return
	}
	respLen := binary.BigEndian.Uint16(hdr[4:6])
	if respLen > 1 {
		_, _ = readFull(conn, make([]byte, int(respLen)-1))
	}
	// Craft a lying response: PDU = fc(0x04) + byteCount(200) + only 4 payload
	// bytes. len(PDU)=6, so MBAP length field = 1 (unit) + 6 = 7.
	pdu := []byte{0x04, 200, 0x00, 0x01, 0x00, 0x02}
	mbap := make([]byte, 7)
	binary.BigEndian.PutUint16(mbap[0:2], binary.BigEndian.Uint16(hdr[0:2])) // echo tid
	binary.BigEndian.PutUint16(mbap[2:4], 0)
	binary.BigEndian.PutUint16(mbap[4:6], uint16(len(pdu)+1))
	mbap[6] = hdr[6] // echo unit id
	_, _ = conn.Write(append(mbap, pdu...))
}

func TestJointModbusShortResponseNoPanic(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go serveLyingModbusInput(t, ln)

	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	var port int
	for _, c := range portStr {
		port = port*10 + int(c-'0')
	}
	d, err := NewModbusTCPDriver("lying-modbus", map[string]interface{}{
		"host": "127.0.0.1", "port": port, "slave_id": 1, "timeout": 2,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer d.Disconnect()

	// "IR100" routes to readInputRegisters (FC04), the function that lacked the
	// guard. Must not panic; must surface as Quality:"bad".
	got, err := d.(*ModbusTCPDriver).ReadPoints(ctx, []models.PointDef{
		{Name: "ir", Address: "IR100", DataType: "int16"},
	})
	if err != nil {
		return // whole-batch error is also acceptable/honest
	}
	if len(got) != 1 {
		t.Fatalf("want 1 row, got %d", len(got))
	}
	if got[0].Quality == "good" {
		t.Fatalf("lying short response produced Quality=good value=%v — fabricated", got[0].Value)
	}
	if got[0].Value != nil {
		t.Fatalf("bad-quality row carried non-nil value %v", got[0].Value)
	}
}

// ---------------------------------------------------------------------------
// OPC UA: reading a node the server does not know must not be reported as a
// good reading. ProtoForge's OPC UA stack returns a genuine BadNodeIdUnknown,
// so this is a real end-to-end honesty check (S7/MC/Modbus servers fabricate
// zeros for unknown addresses and cannot be used for this assertion).
// ---------------------------------------------------------------------------

func TestJointDebug_OPCUABadNodeHonestyLive(t *testing.T) {
	if !pfTCPUp(t, "127.0.0.1:4840") {
		t.Skip("ProtoForge OPC UA simulator not reachable on 127.0.0.1:4840")
	}
	d, err := NewOPCUADriver("jd-ua", map[string]interface{}{
		"endpoint": "opc.tcp://127.0.0.1:4840/freeopcua/server/",
		"timeout":  5,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := d.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer d.Disconnect()

	got, err := d.ReadPoints(ctx, []models.PointDef{
		{Name: "ghost", Address: "ns=50;s=does_not_exist_9d41", DataType: "string"},
	})
	if err != nil {
		return // batch-level error is honest too
	}
	if len(got) != 1 {
		t.Fatalf("want 1 row, got %d", len(got))
	}
	if got[0].Quality == "good" {
		t.Fatalf("read of unknown node reported good value=%v — fabricated", got[0].Value)
	}
}

// ---------------------------------------------------------------------------
// HTTP webhook honesty (hermetic; no ProtoForge needed): the driver stores
// whatever quality the push carries and must never upgrade a null/bad sample to
// "good". This regression covers the fix where HandleWebhook discarded the
// pushed quality and ReadPoints hardcoded "good" for any present key.
// ---------------------------------------------------------------------------

func newWebhookForTest(t *testing.T) *HTTPWebhookDriver {
	t.Helper()
	d, err := NewHTTPWebhookDriver("jd-webhook", map[string]interface{}{})
	if err != nil {
		t.Fatalf("NewHTTPWebhookDriver: %v", err)
	}
	return d.(*HTTPWebhookDriver)
}

func webhookRow(t *testing.T, d *HTTPWebhookDriver, name string) (interface{}, string) {
	t.Helper()
	rows, err := d.ReadPoints(context.Background(), []models.PointDef{{Name: name}})
	if err != nil {
		t.Fatalf("ReadPoints(%s): %v", name, err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row for %s, got %d", name, len(rows))
	}
	return rows[0].Value, rows[0].Quality
}

func TestJointDebug_WebhookQualityHonesty(t *testing.T) {
	// A pushed null with an explicit "bad" quality stays bad (was: hardcoded good).
	d := newWebhookForTest(t)
	if err := d.HandleWebhook([]byte(`[{"point":"p","value":null,"quality":"bad"}]`)); err != nil {
		t.Fatalf("HandleWebhook: %v", err)
	}
	v, q := webhookRow(t, d, "p")
	if q != "bad" {
		t.Fatalf("explicit bad null push -> quality %q, want bad", q)
	}
	if v != nil {
		t.Fatalf("explicit bad null push -> value %v, want nil", v)
	}

	// A pushed null with NO quality field must not be fabricated as good.
	d = newWebhookForTest(t)
	if err := d.HandleWebhook([]byte(`{"point":"p","value":null}`)); err != nil {
		t.Fatalf("HandleWebhook: %v", err)
	}
	_, q = webhookRow(t, d, "p")
	if q == "good" {
		t.Fatalf("null value with no quality reported good — fabricated")
	}

	// A real value with no quality field defaults to good (genuine reading).
	d = newWebhookForTest(t)
	if err := d.HandleWebhook([]byte(`{"temperature":25.5}`)); err != nil {
		t.Fatalf("HandleWebhook: %v", err)
	}
	v, q = webhookRow(t, d, "temperature")
	if q != "good" {
		t.Fatalf("flat non-null value -> quality %q, want good", q)
	}
	if f, ok := v.(float64); !ok || f != 25.5 {
		t.Fatalf("flat value = %v (%T), want float64 25.5", v, v)
	}

	// A pushed "uncertain" quality is honored verbatim, not normalized to good/bad.
	d = newWebhookForTest(t)
	if err := d.HandleWebhook([]byte(`[{"point":"p","value":1,"quality":"uncertain"}]`)); err != nil {
		t.Fatalf("HandleWebhook: %v", err)
	}
	if _, q := webhookRow(t, d, "p"); q != "uncertain" {
		t.Fatalf("pushed uncertain -> %q, want uncertain", q)
	}

	// A point that was never pushed stays "unknown" (not good, not bad).
	d = newWebhookForTest(t)
	if _, q := webhookRow(t, d, "never"); q != "unknown" {
		t.Fatalf("unpushed point -> %q, want unknown", q)
	}
}

// A later push for the same point must overwrite both value and quality, so a
// stale "good" cannot mask a subsequent bad sample.
func TestJointDebug_WebhookQualityOverwrite(t *testing.T) {
	d := newWebhookForTest(t)
	if err := d.HandleWebhook([]byte(`[{"point":"p","value":42,"quality":"good"}]`)); err != nil {
		t.Fatalf("first push: %v", err)
	}
	if _, q := webhookRow(t, d, "p"); q != "good" {
		t.Fatalf("first push quality %q, want good", q)
	}
	if err := d.HandleWebhook([]byte(`[{"point":"p","value":null,"quality":"bad"}]`)); err != nil {
		t.Fatalf("second push: %v", err)
	}
	v, q := webhookRow(t, d, "p")
	if q != "bad" {
		t.Fatalf("overwrite quality %q, want bad (stale good masked the bad sample)", q)
	}
	if v != nil {
		t.Fatalf("overwrite value %v, want nil", v)
	}
}

// ---------------------------------------------------------------------------
// Parse-rejection honesty (hermetic; no ProtoForge, no sockets): every TCP
// protocol must reject a malformed address at parse time, BEFORE any value
// decoder can shape real bytes into a plausible-but-wrong "good" reading. The
// ProtoForge S7/MC/Modbus/FINS servers fabricate zeros for well-formed but
// unknown addresses, so this invariant can only be locked in by driving the
// parsers directly. It always runs.
// ---------------------------------------------------------------------------

func TestJointDebug_MalformedAddressParseRejection(t *testing.T) {
	cases := []struct {
		name  string
		parse func(string) error
		bad   []string
		good  string
	}{
		{
			name:  "modbus",
			parse: func(s string) error { _, _, _, err := parseModbusAddress(s); return err },
			bad:   []string{"", "HR", "NOT_A_REAL_ADDRESS", "4XXXX", "MW"},
			good:  "HR100",
		},
		{
			name:  "s7",
			parse: func(s string) error { _, _, _, _, _, err := parseS7Address(s); return err },
			bad:   []string{"", "@@garbage@@", "DBX", "DB1.DBQ5", "MW"},
			good:  "DB1.DBW0",
		},
		{
			name:  "fins",
			parse: func(s string) error { _, _, _, _, err := parseFINSAddress(s); return err },
			bad:   []string{"", "QQ99", "12345", "NOTREAL"},
			good:  "D2",
		},
		{
			name:  "mc",
			parse: func(s string) error { _, _, err := parseMCAddress(s); return err },
			bad:   []string{"", "NOTADEVICECODE", "500", "@@@"},
			good:  "D100",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.parse(c.good); err != nil {
				t.Fatalf("valid address %q wrongly rejected: %v", c.good, err)
			}
			for _, a := range c.bad {
				if err := c.parse(a); err == nil {
					t.Fatalf("malformed address %q accepted — parser must reject before decode", a)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// OPC UA write-path honesty (live ProtoForge): a write to a node the server
// does not know must return an error, never a silent nil "success". ProtoForge
// has no seeded devices (demo_mode off), so a full write->read-back round trip
// is not possible; the honest failure of a rejected write is the strongest
// guarantee available and it closes OPC-UA's write coverage gap. The driver
// checks WriteResponse.Results[0] against StatusOK, so a BadNodeIdUnknown here
// must surface as a non-nil error rather than a fabricated success.
// ---------------------------------------------------------------------------

func TestJointDebug_OPCUAWriteRejectedHonestyLive(t *testing.T) {
	if !pfTCPUp(t, "127.0.0.1:4840") {
		t.Skip("ProtoForge OPC UA simulator not reachable on 127.0.0.1:4840")
	}
	d, err := NewOPCUADriver("jd-ua-write", map[string]interface{}{
		"endpoint": "opc.tcp://127.0.0.1:4840/freeopcua/server/",
		"timeout":  5,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := d.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer d.Disconnect()

	if err := d.WritePoint(ctx, "ns=50;s=ghost_write_9d41", float64(1.0)); err == nil {
		t.Fatalf("write to unknown node reported success — fabricated write ack")
	}
}

// ---------------------------------------------------------------------------
// Allen-Bradley EtherNet/IP (CIP): write -> read-back (live ProtoForge only).
//
// AB had no round-trip coverage at all before this: the contract test only
// reads, so a WritePoint that acked without the value reaching the PLC would
// have gone unnoticed. Candidate tags are probed rather than hardcoded because
// the simulation serves a set that changes with its config, and a tag that
// drifts between two consecutive reads is excluded: asserting equality against
// a randomized tag would make the test fail for the wrong reason.
// ---------------------------------------------------------------------------

// stableTag picks the first address whose two consecutive reads are both good
// and identical. Volatile tags (a simulation that regenerates its value each
// scan) cannot carry a meaningful write/read-back assertion, so they are
// skipped rather than guessed at.
func stableTag(t *testing.T, read func(addr string, dataType string) (interface{}, bool), candidates []string, dtypes []string) (string, string, interface{}, bool) {
	t.Helper()
	for _, addr := range candidates {
		for _, dt := range dtypes {
			v1, ok1 := read(addr, dt)
			v2, ok2 := read(addr, dt)
			if !ok1 || !ok2 {
				continue
			}
			if fmt.Sprint(v1) == fmt.Sprint(v2) {
				return addr, dt, v1, true
			}
			t.Logf("tag %s as %s is volatile (%v then %v), not a valid write target", addr, dt, v1, v2)
		}
	}
	return "", "", nil, false
}

func TestJointDebug_ABWriteReadBackLive(t *testing.T) {
	pfLive(t, "127.0.0.1:44818")
	d, err := NewABDriver("jd-ab", map[string]interface{}{
		"host": "127.0.0.1", "port": 44818, "plc_type": "ControlLogix", "timeout": 5,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := d.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer d.Disconnect()

	abd := d.(*ABDriver)
	// "auto" makes the driver decode with the CIP type the controller reports,
	// which is what an operator gets for a point with no declared type. Pinning
	// int16 here would re-read a REAL tag's bytes as garbage and then blame the
	// write for the driver's own mis-slice.
	read := func(addr, _ string) (interface{}, bool) {
		rows, err := abd.ReadPoints(ctx, []models.PointDef{{Name: addr, Address: addr, DataType: "auto"}})
		if err != nil || len(rows) != 1 || rows[0].Quality != "good" {
			return nil, false
		}
		return rows[0].Value, true
	}

	tag, _, original, ok := stableTag(t, read,
		[]string{"Setpoint", "Temperature", "Counter"}, []string{"auto"})
	if !ok {
		t.Skip("ProtoForge Allen-Bradley simulation served no stable readable tag - nothing safe to write to")
	}
	cipType, known := abd.tagCipType(tag)
	t.Logf("tag %s reports CIP type 0x%04x (usable=%v), current value %v", tag, cipType, known, original)
	// The written value has to differ from what the tag already holds, or the
	// read-back proves nothing: a stuck-at controller would pass. Candidates are
	// filtered through the driver's own encoder so a narrow tag (SINT) is not
	// asked to take a value that cannot fit it.
	want := 0
	if f, ok := toFloat64(original); ok {
		for _, c := range []int{31337, 31338, 4242, 7, 12, 1} {
			if float64(c) == f {
				continue
			}
			if _, _, err := abd.writePayload(tag, c); err != nil {
				continue
			}
			want = c
			break
		}
	}
	if want == 0 {
		t.Skipf("no candidate write value differs from %v and fits tag %s (CIP 0x%04x)", original, tag, cipType)
	}
	if err := d.WritePoint(ctx, tag, want); err != nil {
		t.Fatalf("WritePoint(%s): %v", tag, err)
	}
	t.Logf("wrote %d to %s, replacing %v", want, tag, original)
	defer func() {
		// Leave the simulation as it was for the next joint-debug session.
		if original != nil {
			if f, ok := toFloat64(original); ok {
				if err := d.WritePoint(ctx, tag, float32(f)); err != nil {
					t.Logf("restore %s=%v: %v", tag, original, err)
				}
			}
		}
	}()

	got, ok := read(tag, "auto")
	if !ok {
		t.Fatalf("read-back of %s after write failed (quality not good)", tag)
	}
	f, numeric := toFloat64(got)
	if !numeric {
		t.Fatalf("read-back of %s produced %v (%T), which cannot be compared", tag, got, got)
	}
	if f != float64(want) {
		t.Fatalf("read-back %v (%T) for CIP type 0x%04x, want %d (tag held %v before) - write did not land",
			got, got, cipType, want, original)
	}
}

// ---------------------------------------------------------------------------
// OPC UA: write -> read-back (live ProtoForge only).
//
// Same gap as AB: the existing OPC-UA joint tests only cover honesty on bad
// nodes, never that a successful write is actually visible on the server.
// Values are written as Go float32 because the simulation's writable nodes are
// typed Float, and a Double variant is refused by the server.
//
// The node to write used to be a hardcoded list of guessed NodeIDs, and when
// none of them existed the test skipped — so it had never once exercised the
// write path. It now asks the server itself, through the driver's Browse
// service, which makes this the live proof of both read and browse.
// ---------------------------------------------------------------------------

// browseMaxNodes bounds the address-space walk so a server with a large
// standard-space tree cannot make the test browse for minutes.
const browseMaxNodes = 200

// browseNumericWritable returns the NodeIDs of the variables a live server says
// it will accept a float for, found with the driver's own Browse service.
//
// It walks only non-zero namespaces: the namespace-0 tree is the standard
// address space (Server status, role sets, alias bookkeeping), which is never
// process data and accounts for hundreds of nodes on any compliant server.
func browseNumericWritable(t *testing.T, browser NodeBrowser, ctx context.Context) []string {
	t.Helper()
	queue := []string{""}
	visited := map[string]bool{}
	seen := 0
	var found []string
	for level := 0; level < 4 && len(queue) > 0 && len(found) < 8; level++ {
		size := len(queue)
		for i := 0; i < size; i++ {
			node := queue[0]
			queue = queue[1:]
			if visited[node] {
				continue
			}
			visited[node] = true
			entries, err := browser.BrowseChildren(ctx, node)
			if err != nil {
				// Browse is a service this build implements; "the server would not
				// answer" is a failure to investigate, not a reason to skip.
				t.Fatalf("Browse(%q): %v", node, err)
			}
			seen += len(entries)
			if seen > browseMaxNodes {
				t.Fatalf("browse walked %d nodes without finding a float-writable variable", seen)
			}
			for _, e := range entries {
				if e.NodeClass == "variable" {
					// The server reports DataType as the ns=0 type name (Float,
					// Double, Boolean, String), so "not Float/Double" is the server
					// declining this node as a float write target.
					if e.Writable && (e.DataType == "Float" || e.DataType == "Double") {
						found = append(found, e.NodeID)
					}
					continue
				}
				if e.IsContainer && !isStandardNamespace(e.NodeID) {
					queue = append(queue, e.NodeID)
				}
			}
		}
	}
	return found
}

// isStandardNamespace reports whether a NodeID belongs to the namespace-0
// address space. gopcua renders those without a prefix at all ("i=2253"), so a
// vendor node is the only one whose text starts with an explicit ns= index.
func isStandardNamespace(nodeID string) bool {
	return !strings.HasPrefix(nodeID, "ns=") || strings.HasPrefix(nodeID, "ns=0;")
}

func TestJointDebug_OPCUAWriteReadBackLive(t *testing.T) {
	pfLive(t, "127.0.0.1:4840")
	d, err := NewOPCUADriver("jd-ua-w", map[string]interface{}{
		"endpoint": "opc.tcp://127.0.0.1:4840/protoforge",
		"timeout":  5,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := d.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer d.Disconnect()

	uad := d.(*OPCUADriver)
	// ReadPoints resolves pt.Address verbatim as a NodeID, so the address is the
	// node string itself; the name is only a result key.
	read := func(addr, dt string) (interface{}, bool) {
		rows, err := uad.ReadPoints(ctx, []models.PointDef{{Name: addr, Address: addr, DataType: dt}})
		if err != nil || len(rows) != 1 || rows[0].Quality != "good" {
			return nil, false
		}
		return rows[0].Value, true
	}

	candidates := browseNumericWritable(t, uad, ctx)
	t.Logf("browse reported %d float-writable node(s): %v", len(candidates), candidates)
	node, _, original, ok := stableTag(t, read, candidates, []string{"float32", "float"})
	if !ok {
		t.Skip("ProtoForge OPC UA simulation exposed no stable readable node - nothing safe to write to")
	}

	want := 33.5
	if _, ok := toFloat64(original); !ok {
		t.Skipf("node %s is not numeric (%v) - float write would be rejected by the server", node, original)
	}
	if err := d.WritePoint(ctx, node, float32(want)); err != nil {
		t.Fatalf("WritePoint(%s): %v", node, err)
	}
	defer func() {
		if orig, ok := toFloat64(original); ok {
			if err := d.WritePoint(ctx, node, float32(orig)); err != nil {
				t.Logf("restore %s=%v: %v", node, orig, err)
			}
		}
	}()

	// Read through a different call so the assertion is about server state, not
	// about whatever the driver cached for the write.
	got, ok := read(node, "float32")
	if !ok {
		t.Fatalf("read-back of %s after write failed (quality not good)", node)
	}
	f, ok := toFloat64(got)
	if !ok || math.Abs(f-want) > 1e-3 {
		t.Fatalf("read-back %v (%T), want %v - write did not land", got, got, want)
	}
}
