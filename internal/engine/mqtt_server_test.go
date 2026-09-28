package engine

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// --- packet builders -------------------------------------------------------

func strField(s string) []byte {
	out := []byte{byte(len(s) >> 8), byte(len(s))}
	return append(out, s...)
}

func connectPacket(clientID, username, password string) []byte {
	body := append([]byte{0x00, 0x04}, "MQTT"...)
	body = append(body, 0x04) // protocol level
	flags := byte(0x02)       // clean session
	if username != "" {
		flags |= 0x80
		if password != "" {
			flags |= 0x40
		}
	}
	body = append(body, flags, 0x00, 0x1e)
	body = append(body, strField(clientID)...)
	if username != "" {
		body = append(body, strField(username)...)
		if password != "" {
			body = append(body, strField(password)...)
		}
	}
	return encodeMQTTPacket(0x10, body)
}

func subscribePacket(id uint16, filters []struct {
	topic string
	qos   byte
}) []byte {
	body := []byte{byte(id >> 8), byte(id)}
	for _, f := range filters {
		body = append(body, strField(f.topic)...)
		body = append(body, f.qos)
	}
	return encodeMQTTPacket(0x82, body)
}

func publishPacket(id uint16, qos byte, topic, payload string) []byte {
	body := strField(topic)
	if qos > 0 {
		body = append(body, byte(id>>8), byte(id))
	}
	body = append(body, payload...)
	return encodeMQTTPacket(0x30|(qos<<1), body)
}

func readPacket(t *testing.T, r *bufio.Reader) mqttPacket {
	t.Helper()
	done := make(chan mqttPacket, 1)
	errCh := make(chan error, 1)
	go func() {
		p, err := readMQTTPacket(r)
		if err != nil {
			errCh <- err
			return
		}
		done <- p
	}()
	select {
	case p := <-done:
		return p
	case err := <-errCh:
		t.Fatalf("read packet failed: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for a packet")
	}
	return mqttPacket{}
}

// startBroker starts a server on an ephemeral loopback port.
func startBroker(t *testing.T, cfg MqttServerConfig) (*MqttServer, string) {
	t.Helper()
	if cfg.Host == "" {
		cfg.Host = "127.0.0.1"
	}
	cfg.Enabled = true
	srv := NewMqttServer()
	ctx, cancel := context.WithCancel(context.Background())
	if err := srv.Start(ctx, cfg); err != nil {
		cancel()
		t.Fatalf("Start failed: %v", err)
	}
	t.Cleanup(func() {
		_ = srv.Stop()
		cancel()
	})
	addr := srv.listener.Addr().String()
	return srv, addr
}

func dialBroker(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// --- tests -----------------------------------------------------------------

// A QoS 1 publisher must get PUBACK and a coalesced PINGREQ must still be
// answered: the broker used to ack nothing and to dispatch only the first
// packet per TCP read, which froze the northbound path ("pingresp not
// received, disconnecting" every keepalive).
func TestMqttServerAcksQoS1AndAnswersCoalescedPing(t *testing.T) {
	_, addr := startBroker(t, MqttServerConfig{Port: 0, MaxClients: 10, AllowNoAuth: true})

	sub := dialBroker(t, addr)
	subReader := bufio.NewReader(sub)
	if _, err := sub.Write(connectPacket("sub-1", "", "")); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	if p := readPacket(t, subReader); p.packetType() != 2 {
		t.Fatalf("expected CONNACK, got type %d", p.packetType())
	}
	filters := []struct {
		topic string
		qos   byte
	}{{"edgelite/#", 1}, {"other/+/topic", 2}}
	if _, err := sub.Write(subscribePacket(7, filters)); err != nil {
		t.Fatalf("write SUBSCRIBE: %v", err)
	}
	// Both filters must be accepted, and a QoS 2 request clamped to QoS 1: the
	// old handler registered only the first filter and always granted QoS 0.
	suback := readPacket(t, subReader)
	if suback.packetType() != 9 {
		t.Fatalf("expected SUBACK, got type %d", suback.packetType())
	}
	if len(suback.body) != 4 || suback.body[2] != 1 || suback.body[3] != 1 {
		t.Fatalf("SUBACK must grant both filters at QoS 1, got %v", suback.body)
	}

	pub := dialBroker(t, addr)
	pubReader := bufio.NewReader(pub)
	if _, err := pub.Write(connectPacket("pub-1", "", "")); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	readPacket(t, pubReader) // CONNACK

	if _, err := pub.Write(publishPacket(42, 1, "edgelite/devices/d1/data", "hello")); err != nil {
		t.Fatalf("write PUBLISH: %v", err)
	}
	// PINGREQ piggy-backed on the same write: it used to be swallowed whole.
	if _, err := pub.Write(append(encodeMQTTPacket(0xC0, nil), encodeMQTTPacket(0xC0, nil)...)); err != nil {
		t.Fatalf("write PINGREQ: %v", err)
	}

	ack := readPacket(t, pubReader)
	if ack.packetType() != 4 {
		t.Fatalf("expected PUBACK for QoS 1 publish, got type %d", ack.packetType())
	}
	if len(ack.body) != 2 || ack.body[0] != 0 || ack.body[1] != 42 {
		t.Fatalf("PUBACK must echo packet id 42, got %v", ack.body)
	}
	for i := 0; i < 2; i++ {
		if p := readPacket(t, pubReader); p.packetType() != 13 {
			t.Fatalf("expected PINGRESP %d, got type %d", i+1, p.packetType())
		}
	}

	// The wildcard subscriber must actually receive the forwarded publish.
	forwarded := readPacket(t, subReader)
	if forwarded.packetType() != 3 {
		t.Fatalf("expected forwarded PUBLISH via edgelite/#, got type %d", forwarded.packetType())
	}
	if !strings.Contains(string(forwarded.body), "edgelite/devices/d1/data") ||
		!strings.Contains(string(forwarded.body), "hello") {
		t.Fatalf("forwarded packet lost topic or payload: %q", forwarded.body)
	}
}

// Credentials in the config have to be enforced: AuthMode was never derived
// from them, so every client was accepted.
func TestMqttServerRejectsBadCredentials(t *testing.T) {
	_, addr := startBroker(t, MqttServerConfig{
		Port: 0, MaxClients: 10, Username: "gateway", Password: "s3cret",
	})

	for _, tc := range []struct {
		name           string
		user, pass     string
		wantReturnCode byte
	}{
		{"wrong password", "gateway", "nope", 4},
		{"no credentials", "", "", 4},
		{"correct", "gateway", "s3cret", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := dialBroker(t, addr)
			r := bufio.NewReader(conn)
			if _, err := conn.Write(connectPacket("c-"+tc.name, tc.user, tc.pass)); err != nil {
				t.Fatalf("write CONNECT: %v", err)
			}
			p := readPacket(t, r)
			if p.packetType() != 2 {
				t.Fatalf("expected CONNACK, got type %d", p.packetType())
			}
			if len(p.body) != 2 || p.body[1] != tc.wantReturnCode {
				t.Fatalf("return code = %v, want %d", p.body, tc.wantReturnCode)
			}
		})
	}
}

// The management API used to hardcode an empty client list and a successful
// kick that closed nothing, so the module page always looked idle.
func TestMqttServerListAndKickClients(t *testing.T) {
	srv, addr := startBroker(t, MqttServerConfig{Port: 0, MaxClients: 10, AllowNoAuth: true})

	sub := dialBroker(t, addr)
	subReader := bufio.NewReader(sub)
	if _, err := sub.Write(connectPacket("sub-1", "", "")); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	readPacket(t, subReader) // CONNACK
	if _, err := sub.Write(subscribePacket(3, []struct {
		topic string
		qos   byte
	}{{"edgelite/#", 1}})); err != nil {
		t.Fatalf("write SUBSCRIBE: %v", err)
	}
	readPacket(t, subReader) // SUBACK

	pub := dialBroker(t, addr)
	pubReader := bufio.NewReader(pub)
	if _, err := pub.Write(connectPacket("pub-1", "", "")); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	readPacket(t, pubReader) // CONNACK

	clients := srv.ListClients()
	if len(clients) != 2 || clients[0].ClientID != "pub-1" || clients[1].ClientID != "sub-1" {
		t.Fatalf("both clients must be listed in id order, got %+v", clients)
	}
	if len(clients[1].Subscriptions) != 1 || clients[1].Subscriptions[0] != "edgelite/#" {
		t.Fatalf("subscription missing from client list: %+v", clients[1].Subscriptions)
	}
	if clients[0].RemoteAddr == "" || clients[0].ConnectedAt.IsZero() {
		t.Fatalf("client list must carry connection metadata: %+v", clients[0])
	}

	if err := srv.KickClient("does-not-exist"); err == nil {
		t.Fatal("kicking an unknown client must report an error, not success")
	}
	if err := srv.KickClient("pub-1"); err != nil {
		t.Fatalf("KickClient failed: %v", err)
	}
	// The kicked side sees a closed connection rather than a silent stall.
	if _, err := pubReader.ReadByte(); err == nil {
		t.Fatal("kicked client should have no data left to read")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		remaining := srv.ListClients()
		if len(remaining) == 1 && remaining[0].ClientID == "sub-1" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("kicked client was not deregistered, remaining=%+v", remaining)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestMqttTopicMatches(t *testing.T) {
	cases := []struct {
		filter, topic string
		want          bool
	}{
		{"a/b", "a/b", true},
		{"a/#", "a/b/c", true},
		{"a/#", "a", true},
		{"a/#", "b/c", false},
		{"a/+/c", "a/b/c", true},
		{"a/+/c", "a/b/b/c", false},
		{"a/b", "a/b/c", false},
		{"#", "any/thing", true},
	}
	for _, tc := range cases {
		if got := mqttTopicMatches(tc.filter, tc.topic); got != tc.want {
			t.Errorf("mqttTopicMatches(%q, %q) = %v, want %v", tc.filter, tc.topic, got, tc.want)
		}
	}
}

func TestEncodeMQTTPacketRemainingLength(t *testing.T) {
	// A body of 300 bytes needs the two-byte varint 0xAC 0x02.
	body := make([]byte, 300)
	p := encodeMQTTPacket(0x30, body)
	if len(p) != 303 || p[0] != 0x30 || p[1] != 0xAC || p[2] != 0x02 {
		t.Fatalf("unexpected framing: len=%d head=%v", len(p), p[:3])
	}
	r := bufio.NewReader(strings.NewReader(string(p)))
	got, err := readMQTTPacket(r)
	if err != nil {
		t.Fatalf("read back failed: %v", err)
	}
	if got.hdr != 0x30 || len(got.body) != 300 {
		t.Fatalf("round trip lost data: hdr=%#x len=%d", got.hdr, len(got.body))
	}
}

// A broker configured with a credential mode but no secret must refuse to
// listen rather than accept anonymous clients; and it must stay startable
// afterwards (the validation used to return while holding the mutex, and
// marked the server started before the bind was attempted).
func TestMqttServerStartFailsClosedOnWeakAuth(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  MqttServerConfig
		want string
	}{
		{"basic without password", MqttServerConfig{AuthMode: "basic", Username: "gw"}, "needs both username and password"},
		{"basic without username", MqttServerConfig{AuthMode: "basic", Password: "pw"}, "needs both username and password"},
		// allow_no_auth: false is a request for authentication. Empty credentials
		// used to be read as "no credentials configured, so run anonymously",
		// which silently dropped that request.
		{"auth required but no credentials", MqttServerConfig{Username: "", Password: "", AllowNoAuth: false}, "needs both username and password"},
		{"token without token", MqttServerConfig{AuthMode: "token"}, "needs a non-empty token"},
		{"unknown mode", MqttServerConfig{AuthMode: "mtls"}, "is not one of none/basic/token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := NewMqttServer()
			type result struct{ err error }
			done := make(chan result, 1)
			go func() { done <- result{srv.Start(context.Background(), tc.cfg)} }()
			select {
			case res := <-done:
				if res.err == nil {
					t.Fatal("Start must fail")
				}
				if !strings.Contains(res.err.Error(), tc.want) {
					t.Fatalf("error %q does not mention %q", res.err, tc.want)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("Start deadlocked or blocked instead of returning")
			}
			if srv.IsRunning() {
				t.Fatal("server must not report running after a failed start")
			}
			// The same instance must be reusable: a failed start used to leave
			// started=true, so the operator could never fix the config in-process.
			good := tc.cfg
			good.Password = "pw"
			good.Username = "gw"
			good.Token = "tk"
			good.AuthMode = ""
			if err := srv.Start(context.Background(), good); err != nil {
				t.Fatalf("restart with valid credentials failed: %v", err)
			}
			_ = srv.Stop()
		})
	}
}

// A bind failure must not leave the server marked as running either.
func TestMqttServerStartRollsBackOnBindFailure(t *testing.T) {
	_, addr := startBroker(t, MqttServerConfig{Port: 0, AllowNoAuth: true})
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %q: %v", addr, err)
	}
	port := 0
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		t.Fatalf("parse port %q: %v", portStr, err)
	}

	srv := NewMqttServer()
	if err := srv.Start(context.Background(), MqttServerConfig{Host: host, Port: port, AllowNoAuth: true}); err == nil {
		t.Fatal("starting on an occupied port must fail")
	}
	if srv.IsRunning() {
		t.Fatal("failed bind must not leave the server running")
	}
	if err := srv.Start(context.Background(), MqttServerConfig{Host: "127.0.0.1", Port: 0, AllowNoAuth: true}); err != nil {
		t.Fatalf("server must be startable after a failed bind: %v", err)
	}
	_ = srv.Stop()
}

// Token mode must accept the token in either credential field and reject
// anything else, including the empty credentials an anonymous client sends.
func TestMqttServerTokenModeAuth(t *testing.T) {
	_, addr := startBroker(t, MqttServerConfig{Port: 0, MaxClients: 10, AuthMode: "token", Token: "tok-1"})

	for _, tc := range []struct {
		name       string
		user, pass string
		want       byte
	}{
		{"token as username", "tok-1", "", 0},
		{"token as password", "some-client", "tok-1", 0},
		{"anonymous", "", "", 4},
		{"wrong token", "tok-2", "tok-2", 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := dialBroker(t, addr)
			r := bufio.NewReader(conn)
			if _, err := conn.Write(connectPacket("tok-"+tc.name, tc.user, tc.pass)); err != nil {
				t.Fatalf("write CONNECT: %v", err)
			}
			p := readPacket(t, r)
			if p.packetType() != 2 || len(p.body) != 2 || p.body[1] != tc.want {
				t.Fatalf("CONNACK = %v, want return code %d", p.body, tc.want)
			}
		})
	}
}
