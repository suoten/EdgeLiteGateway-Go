package drivers

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"edgelite/internal/models"
)

// The MQTTClientDriver speaks raw MQTT 3.1.1 over TCP (it does NOT use the
// paho.mqtt.golang dependency that is present in go.mod) and its ingest surface
// is the exported HandleMessage method. The HTTPWebhookDriver is a pure
// receiving/parse surface: it exposes HandleWebhook(payload []byte) error, while
// the actual HTTP route lives outside this package. These tests therefore prove
// each driver's real API contract: (a) a live CONNECT/CONNACK handshake + QoS0
// PUBLISH against an in-process mock broker, (b) message ingest -> quality
// mapping, and (c) webhook POST -> parse -> read, including graceful rejection
// of malformed payloads. A live ProtoForge (127.0.0.1:1883) smoke test skips
// when no broker is reachable.

// --- in-process mock MQTT broker (minimal CONNECT/CONNACK/PUBLISH/PING/DISCONNECT) ---

type pfCapturedPublish struct {
	topic   string
	payload []byte
}

type pfMockBroker struct {
	ln       net.Listener
	mu       sync.Mutex
	connacks int
	pubs     []pfCapturedPublish
}

func newPFMockBroker(t *testing.T) (*pfMockBroker, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	b := &pfMockBroker{ln: ln}
	go b.serve()
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port := 0
	for _, c := range portStr {
		port = port*10 + int(c-'0')
	}
	return b, port
}

func (b *pfMockBroker) serve() {
	for {
		conn, err := b.ln.Accept()
		if err != nil {
			return
		}
		go b.handle(conn)
	}
}

func (b *pfMockBroker) handle(conn net.Conn) {
	defer conn.Close()
	for {
		ctrl, err := pfReadByte(conn)
		if err != nil {
			return
		}
		remaining, err := pfReadRemainingLength(conn)
		if err != nil {
			return
		}
		body := make([]byte, remaining)
		if remaining > 0 {
			if _, err := io.ReadFull(conn, body); err != nil {
				return
			}
		}
		switch ctrl & 0xF0 {
		case 0x10: // CONNECT -> reply CONNACK accepted
			// Increment the counter BEFORE sending CONNACK: the client's Connect()
			// returns as soon as it reads CONNACK, so a post-write increment would
			// let a caller observe IsConnected()==true with connacks still 0 (race).
			b.mu.Lock()
			b.connacks++
			b.mu.Unlock()
			if _, err := conn.Write([]byte{0x20, 0x02, 0x00, 0x00}); err != nil {
				return
			}
		case 0x30: // PUBLISH (QoS in bits 1-2)
			qos := (ctrl >> 1) & 0x03
			pfHandlePublish(b, body, qos)
		case 0xC0: // PINGREQ -> PINGRESP
			if _, err := conn.Write([]byte{0xD0, 0x00}); err != nil {
				return
			}
		case 0xE0: // DISCONNECT
			return
		}
	}
}

func pfHandlePublish(b *pfMockBroker, body []byte, qos byte) {
	if len(body) < 2 {
		return
	}
	tlen := int(body[0])<<8 | int(body[1])
	if len(body) < 2+tlen {
		return
	}
	topic := string(body[2 : 2+tlen])
	rest := body[2+tlen:]
	if qos > 0 {
		if len(rest) < 2 {
			return
		}
		rest = rest[2:]
	}
	payload := append([]byte(nil), rest...)
	b.mu.Lock()
	b.pubs = append(b.pubs, pfCapturedPublish{topic: topic, payload: payload})
	b.mu.Unlock()
}

// waitPublish blocks until the broker has captured at least want publishes and
// returns the want-th one (1-indexed). Returning "the last of any count" would
// let a stale earlier publish satisfy a later assertion under load, so callers
// pin the expected sequence number.
func (b *pfMockBroker) waitPublish(t *testing.T, want int) pfCapturedPublish {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		b.mu.Lock()
		n := len(b.pubs)
		var p pfCapturedPublish
		if n >= want {
			p = b.pubs[want-1]
		}
		b.mu.Unlock()
		if n >= want {
			return p
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for broker to capture publish #%d (have %d)", want, func() int {
		b.mu.Lock()
		defer b.mu.Unlock()
		return len(b.pubs)
	}())
	return pfCapturedPublish{}
}

func pfReadByte(conn net.Conn) (byte, error) {
	buf := make([]byte, 1)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return 0, err
	}
	return buf[0], nil
}

func pfReadRemainingLength(conn net.Conn) (int, error) {
	multiplier := 1
	value := 0
	for {
		b, err := pfReadByte(conn)
		if err != nil {
			return 0, err
		}
		value += int(b&127) * multiplier
		if b&128 == 0 {
			return value, nil
		}
		multiplier *= 128
		if multiplier > 128*128*128 {
			return 0, io.ErrUnexpectedEOF
		}
	}
}

// --- MQTT handshake + publish over a real TCP socket (hermetic) ---

func TestMQTTClientHandshakeAndPublishAgainstMockBroker(t *testing.T) {
	broker, port := newPFMockBroker(t)
	defer broker.ln.Close()

	d, err := NewMQTTClientDriver("pfmqtt", map[string]interface{}{
		"broker": "127.0.0.1", "port": port, "timeout": 3,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	md := d.(*MQTTClientDriver)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := md.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer md.Disconnect()

	if !md.IsConnected() {
		t.Fatalf("driver should report connected after CONNACK")
	}
	broker.mu.Lock()
	gotConnAck := broker.connacks
	broker.mu.Unlock()
	if gotConnAck != 1 {
		t.Fatalf("broker saw %d CONNACKs, want 1 (real handshake did not occur)", gotConnAck)
	}

	// Numeric value -> valid JSON, published to the default command topic.
	if err := md.WritePoint(ctx, "setpoint", 42); err != nil {
		t.Fatalf("write: %v", err)
	}
	pub := broker.waitPublish(t, 1)
	if want := "edgelite/pfmqtt/command"; pub.topic != want {
		t.Fatalf("publish topic = %q, want %q", pub.topic, want)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(pub.payload, &got); err != nil {
		t.Fatalf("published payload is not valid JSON (%q): %v", string(pub.payload), err)
	}
	if got["point"] != "setpoint" {
		t.Fatalf("published point = %v, want setpoint", got["point"])
	}
	if v, ok := got["value"].(float64); !ok || v != 42 {
		t.Fatalf("published value = %v, want 42", got["value"])
	}

	// String value must be JSON-quoted (regression for the %v encoding bug).
	if err := md.WritePoint(ctx, "mode", "auto"); err != nil {
		t.Fatalf("write: %v", err)
	}
	pub2 := broker.waitPublish(t, 2)
	if !json.Valid(pub2.payload) {
		t.Fatalf("string-valued publish produced invalid JSON: %q", string(pub2.payload))
	}
	var got2 map[string]interface{}
	if err := json.Unmarshal(pub2.payload, &got2); err != nil {
		t.Fatalf("re-decode string publish: %v", err)
	}
	if got2["value"] != "auto" {
		t.Fatalf("string value = %v, want auto", got2["value"])
	}
}

// Reconnect guard: calling Connect twice must not error or leak a socket.
func TestMQTTClientConnectIsIdempotent(t *testing.T) {
	broker, port := newPFMockBroker(t)
	defer broker.ln.Close()
	d, _ := NewMQTTClientDriver("pfmqtt2", map[string]interface{}{
		"broker": "127.0.0.1", "port": port, "timeout": 3,
	})
	md := d.(*MQTTClientDriver)
	ctx := context.Background()
	if err := md.Connect(ctx); err != nil {
		t.Fatalf("first connect: %v", err)
	}
	defer md.Disconnect()
	if err := md.Connect(ctx); err != nil {
		t.Fatalf("second connect should be idempotent, got: %v", err)
	}
	broker.mu.Lock()
	n := broker.connacks
	broker.mu.Unlock()
	if n != 1 {
		t.Fatalf("expected exactly 1 CONNECT handshake, got %d (reconnect guard failed)", n)
	}
}

// TestMQTTUnreachableBrokerFailsConnect pins the removal of the driver's
// "simulated mode": a device pointed at a dead broker used to stay online with
// an empty value set forever, which is indistinguishable from a quiet topic.
func TestMQTTUnreachableBrokerFailsConnect(t *testing.T) {
	d, _ := NewMQTTClientDriver("dead", map[string]interface{}{
		"broker": "127.0.0.1", "port": 1, "timeout": 1,
	})
	md := d.(*MQTTClientDriver)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := md.Connect(ctx); err == nil {
		t.Fatal("Connect to an unreachable broker must fail")
	}
	if md.IsConnected() {
		t.Fatal("failed Connect must leave the driver disconnected")
	}
	if _, err := md.ReadPoints(ctx, []models.PointDef{{Name: "temperature"}}); err == nil {
		t.Fatal("ReadPoints on a disconnected driver must fail")
	}
}

// --- MQTT ingest -> quality mapping (mock broker, no real service needed) ---
func TestMQTTClientIngestAndQualityMapping(t *testing.T) {
	_, port := newPFMockBroker(t)
	d, _ := NewMQTTClientDriver("ing", map[string]interface{}{
		"broker": "127.0.0.1", "port": port, "timeout": 2,
	})
	md := d.(*MQTTClientDriver)
	ctx := context.Background()
	if err := md.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer md.Disconnect()
	if !md.IsConnected() {
		t.Fatalf("driver must report connected after a successful handshake")
	}

	md.HandleMessage("sensor/1", []byte(`{"temperature":25.5,"humidity":60}`))

	pts := []models.PointDef{
		{Name: "temperature"},
		{Name: "humidity"},
		{Name: "missing"},
	}
	got, err := md.ReadPoints(ctx, pts)
	if err != nil {
		t.Fatalf("ReadPoints: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 points, got %d", len(got))
	}
	byName := map[string]struct {
		value   interface{}
		quality string
	}{}
	for _, p := range got {
		byName[p.PointName] = struct {
			value   interface{}
			quality string
		}{p.Value, p.Quality}
	}
	if byName["temperature"].quality != "good" || byName["temperature"].value != 25.5 {
		t.Fatalf("temperature = %v/%s, want 25.5/good", byName["temperature"].value, byName["temperature"].quality)
	}
	if byName["humidity"].quality != "good" {
		t.Fatalf("humidity quality = %s, want good", byName["humidity"].quality)
	}
	if byName["missing"].quality != "unknown" || byName["missing"].value != nil {
		t.Fatalf("missing point = %v/%s, want <nil>/unknown", byName["missing"].value, byName["missing"].quality)
	}
}

// Non-JSON payloads are stored under the topic key as a raw string (documented
// behavior), and a nil payload must not panic.
func TestMQTTClientHandleMessageRawAndNil(t *testing.T) {
	_, port := newPFMockBroker(t)
	d, _ := NewMQTTClientDriver("raw", map[string]interface{}{"broker": "127.0.0.1", "port": port, "timeout": 2})
	md := d.(*MQTTClientDriver)
	ctx := context.Background()
	if err := md.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer md.Disconnect()

	md.HandleMessage("plain/topic", []byte("hello"))
	md.HandleMessage("nil/topic", nil) // must not panic

	md.HandleMessage("plain/topic", []byte("hello"))
	md.HandleMessage("nil/topic", nil) // must not panic

	got, err := md.ReadPoints(ctx, []models.PointDef{{Name: "plain/topic"}, {Name: "nil/topic"}})
	if err != nil {
		t.Fatalf("ReadPoints: %v", err)
	}
	if got[0].Value != "hello" || got[0].Quality != "good" {
		t.Fatalf("raw topic value = %v/%s, want hello/good", got[0].Value, got[0].Quality)
	}
	if got[1].Quality != "good" {
		t.Fatalf("nil-payload topic should be present (empty string), got quality %s", got[1].Quality)
	}
}

// --- HTTP webhook: POST -> parse -> read (hermetic via httptest) ---

func TestHTTPWebhookIngestViaPOST(t *testing.T) {
	d, err := NewHTTPWebhookDriver("wh", map[string]interface{}{"api_key": "secret"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	wd := d.(*HTTPWebhookDriver)

	// Stand up a listener whose handler mirrors how ProtoForge would deliver a
	// telemetry POST body into the driver's real ingest method.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := wd.HandleWebhook(body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(`{"temp":25.5,"mode":"auto"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid payload status = %d, want 200", resp.StatusCode)
	}

	ctx := context.Background()
	got, err := wd.ReadPoints(ctx, []models.PointDef{{Name: "temp"}, {Name: "mode"}})
	if err != nil {
		t.Fatalf("ReadPoints: %v", err)
	}
	if got[0].Value != 25.5 || got[0].Quality != "good" {
		t.Fatalf("temp = %v/%s, want 25.5/good", got[0].Value, got[0].Quality)
	}
	if got[1].Value != "auto" || got[1].Quality != "good" {
		t.Fatalf("mode = %v/%s, want auto/good", got[1].Value, got[1].Quality)
	}

	// Fresh data => healthy.
	if err := wd.HealthCheck(ctx); err != nil {
		t.Fatalf("HealthCheck after ingest: %v", err)
	}

	// Malformed payload => 400 and previously ingested values untouched.
	resp2, err := http.Post(srv.URL, "application/json", strings.NewReader(`{not json`))
	if err != nil {
		t.Fatalf("post malformed: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed payload status = %d, want 400", resp2.StatusCode)
	}
	got2, _ := wd.ReadPoints(ctx, []models.PointDef{{Name: "temp"}})
	if got2[0].Value != 25.5 {
		t.Fatalf("malformed POST changed value: %v", got2[0].Value)
	}
}

// A webhook that has never received data is unhealthy (stale lastUpdate).
func TestHTTPWebhookHealthCheckStale(t *testing.T) {
	d, _ := NewHTTPWebhookDriver("stale", map[string]interface{}{})
	wd := d.(*HTTPWebhookDriver)
	if err := wd.HealthCheck(context.Background()); err == nil {
		t.Fatalf("expected unhealthy for a driver that never received data")
	}
}

// Direct HandleWebhook must reject malformed JSON and accept valid JSON.
func TestHTTPWebhookHandleWebhookParse(t *testing.T) {
	d, _ := NewHTTPWebhookDriver("parse", map[string]interface{}{})
	wd := d.(*HTTPWebhookDriver)
	if err := wd.HandleWebhook([]byte(`bad`)); err == nil {
		t.Fatalf("expected error for malformed JSON")
	}
	if err := wd.HandleWebhook(nil); err == nil {
		t.Fatalf("expected error for nil payload")
	}
	if err := wd.HandleWebhook([]byte(`{"x":1}`)); err != nil {
		t.Fatalf("valid JSON rejected: %v", err)
	}
}

// --- Live ProtoForge MQTT broker smoke test (skips when unreachable) ---

func TestMQTTClientAgainstProtoForge(t *testing.T) {
	probe, err := net.DialTimeout("tcp", "127.0.0.1:1883", 500*time.Millisecond)
	if err != nil {
		t.Skipf("ProtoForge MQTT broker not reachable: %v", err)
	}
	probe.Close()

	d, err := NewMQTTClientDriver("pfmqtt-live", map[string]interface{}{
		"broker": "127.0.0.1", "port": 1883, "timeout": 3,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	md := d.(*MQTTClientDriver)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := md.Connect(ctx); err != nil {
		t.Fatalf("connect to ProtoForge broker: %v", err)
	}
	defer md.Disconnect()
	if !md.IsConnected() {
		t.Fatalf("driver not connected after handshake")
	}

	// Ingest via the driver's real API surface and read the value back.
	md.HandleMessage("edgelite/pfmqtt-live/data", []byte(`{"live_temp":12.3}`))
	got, err := md.ReadPoints(ctx, []models.PointDef{{Name: "live_temp"}})
	if err != nil {
		t.Fatalf("ReadPoints: %v", err)
	}
	if len(got) != 1 || got[0].Quality != "good" || got[0].Value != 12.3 {
		t.Fatalf("live ingest read mismatch: %+v", got)
	}

	// Publishing over the live socket must not error.
	if err := md.WritePoint(ctx, "cmd", 1); err != nil {
		t.Fatalf("live publish: %v", err)
	}
}
