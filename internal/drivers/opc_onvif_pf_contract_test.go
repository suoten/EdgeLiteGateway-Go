package drivers

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"edgelite/internal/models"
)

// This file pins the OPC-family drivers (internal/drivers/opc_onvif.go) to their
// TRUE, verified behaviour against the live ProtoForge simulator plus hermetic
// fallbacks so the contract is asserted with or without a simulator.
//
// Ground truth established by joint debugging:
//   - OPC UA   : a real freeopcua server listens on 127.0.0.1:4840. The driver
//                now uses github.com/gopcua/opcua, which performs the full
//                binary-transport handshake (HEL/ACK, OpenSecureChannel,
//                CreateSession, ActivateSession), so reads succeed against a
//                compliant server. The live test asserts a genuine "good" read
//                of a mandatory NodeId that every OPC UA server must serve.
//   - OPC DA   : ProtoForge listens on 51340, but OPC DA is Windows-only
//                COM/DCOM. The Go OPCDADriver reports the gap directly: every
//                operation, Connect included, returns ErrOPCDAUnsupported and it
//                never claims to be connected. Asserted as such.
//   - ONVIF    : ProtoForge serves no ONVIF SOAP endpoint (80/8000 are web UIs,
//                10000 resets, 51340 drops non-DCOM). ONVIF is therefore NOT
//                tested against the simulator; instead the SOAP request/auth
//                building is proven hermetically against an in-process mock.
//                An unreachable camera fails Connect (there is no "simulated
//                mode") and writes return ErrONVIFWriteUnsupported.

// ---------------------------------------------------------------------------
// OPC UA
// ---------------------------------------------------------------------------

// TestParseOPCUAEndpointProtoForgeContract locks in endpoint/host/port extraction,
// including the "/freeopcua/server/" URL path the ProtoForge simulator exposes.
func TestParseOPCUAEndpointProtoForgeContract(t *testing.T) {
	cases := []struct {
		in       string
		wantHost string
		wantPort int
	}{
		{"opc.tcp://127.0.0.1:4840/freeopcua/server/", "127.0.0.1", 4840},
		{"opc.tcp://127.0.0.1:4840", "127.0.0.1", 4840},
		{"opc.tcp://10.0.0.5:4841/some/path", "10.0.0.5", 4841},
	}
	for _, c := range cases {
		host, port := parseOPCUAEndpoint(c.in)
		if host != c.wantHost || port != c.wantPort {
			t.Errorf("parseOPCUAEndpoint(%q) = (%q,%d), want (%q,%d)", c.in, host, port, c.wantHost, c.wantPort)
		}
	}
}

// TestOPCUAAgainstProtoForge is the live joint-debugging smoke test against the
// running ProtoForge OPC UA simulator on 127.0.0.1:4840. The driver now speaks
// the full OPC UA stack via gopcua (secure channel + session), so it must return
// a genuine "good" value for a mandatory NodeId that every compliant server
// provides (the Server object, ns=0;i=2256). It skips when no simulator is
// reachable so CI stays green.
func TestOPCUAAgainstProtoForge(t *testing.T) {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:4840", 500*time.Millisecond)
	if err != nil {
		t.Skipf("ProtoForge OPC UA simulator not reachable on 127.0.0.1:4840: %v", err)
	}
	conn.Close()

	d, err := NewOPCUADriver("pf-ua", map[string]interface{}{
		"endpoint": "opc.tcp://127.0.0.1:4840/freeopcua/server/",
		"timeout":  5,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	uad := d.(*OPCUADriver)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	if err := uad.Connect(ctx); err != nil {
		t.Fatalf("Connect to ProtoForge OPC UA failed: %v", err)
	}
	defer uad.Disconnect()
	uad.mu.Lock()
	hasClient := uad.client != nil
	uad.mu.Unlock()
	if !hasClient {
		t.Fatal("expected an established gopcua session against the live server")
	}

	// ns=0;i=2256 (Server) is a mandatory node every OPC UA server must serve;
	// reading it proves the full OpenSecureChannel + CreateSession pipeline works
	// end-to-end rather than returning a driver-fabricated value.
	pts := []models.PointDef{{Name: "Server", Address: "ns=0;i=2256", DataType: "string"}}
	got, err := uad.ReadPoints(ctx, pts)
	if err != nil {
		t.Fatalf("ReadPoints: %v", err)
	}
	if len(got) != 1 || got[0].Quality != "good" || got[0].Value == nil {
		t.Fatalf("expected a good-quality read of the mandatory Server node, got %+v", got)
	}
	t.Logf("live OPC UA read Server -> value=%v", got[0].Value)
}

// ---------------------------------------------------------------------------
// OPC DA — graceful degradation only (Windows COM/DCOM, not reachable from Go)
// ---------------------------------------------------------------------------

// TestOPCDAHonestUnsupported pins the driver's TRUE state: OPC DA is a
// Windows-only COM/DCOM protocol that a Go client cannot speak natively, so
// every entry point must return ErrOPCDAUnsupported. It must not report
// "connected" (that made every OPC DA device look healthy in the UI while
// nothing could ever reach a server) and must not fabricate values.
// ProtoForge's 51340 listener speaks raw DCOM framing, not anything a Go client
// can interoperate with, so no live test is possible here.
func TestOPCDAHonestUnsupported(t *testing.T) {
	d, err := NewOPCDADriver("da-1", map[string]interface{}{"server": "Matrikon.OPC.Simulation"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := d.Connect(ctx); !errors.Is(err, ErrOPCDAUnsupported) {
		t.Fatalf("opc_da Connect must fail with ErrOPCDAUnsupported, got %v", err)
	}
	pts := []models.PointDef{{Name: "Level", Address: "Random.Int4", DataType: "int32"}}
	if got, err := d.ReadPoints(ctx, pts); !errors.Is(err, ErrOPCDAUnsupported) || got != nil {
		t.Fatalf("opc_da ReadPoints must fail with ErrOPCDAUnsupported, got %v / %v", got, err)
	}
	if err := d.WritePoint(ctx, "Level", 1); !errors.Is(err, ErrOPCDAUnsupported) {
		t.Fatalf("opc_da WritePoint must fail with ErrOPCDAUnsupported, got %v", err)
	}
	if got, err := d.Discover(ctx, nil); !errors.Is(err, ErrOPCDAUnsupported) || got != nil {
		t.Fatalf("opc_da Discover must fail with ErrOPCDAUnsupported, got %v / %v", got, err)
	}
	if err := d.HealthCheck(ctx); !errors.Is(err, ErrOPCDAUnsupported) {
		t.Fatalf("opc_da HealthCheck must fail with ErrOPCDAUnsupported, got %v", err)
	}
	if d.IsConnected() {
		t.Fatal("opc_da must never report itself connected")
	}
	// The error text is surfaced straight to the UI, so it has to name the
	// missing piece rather than a generic failure.
	if !strings.Contains(ErrOPCDAUnsupported.Error(), "COM/DCOM") {
		t.Fatalf("ErrOPCDAUnsupported should explain the gap, got %q", ErrOPCDAUnsupported.Error())
	}
}

// ---------------------------------------------------------------------------
// ONVIF — hermetic SOAP request/auth building (no live sim endpoint)
// ---------------------------------------------------------------------------

// TestONVIFReadAgainstMockSOAP proves the SOAP request construction, the
// WS-Security (PasswordDigest) header, and response parsing work end-to-end
// against an in-process mock, since ProtoForge serves no real ONVIF endpoint.
func TestONVIFReadAgainstMockSOAP(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/onvif/device_service" {
			t.Errorf("unexpected ONVIF path %q", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); !strings.Contains(ct, "application/soap+xml") {
			t.Errorf("expected soap+xml content-type, got %q", ct)
		}
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/soap+xml")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `<s:Envelope><s:Body><tds:GetDeviceInformationResponse>`+
			`<tds:Manufacturer>EdgeLite-Sim</tds:Manufacturer>`+
			`<tds:Model>PF-ONVIF</tds:Model>`+
			`</tds:GetDeviceInformationResponse></s:Body></s:Envelope>`)
	}))
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	host, portStr, _ := net.SplitHostPort(u.Host)
	port, _ := strconv.Atoi(portStr)

	d, err := NewONVIFDriver("onvif-1", map[string]interface{}{
		"host": host, "port": port, "username": "admin", "password": "secret", "timeout": 3,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	onv := d.(*ONVIFDriver)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := onv.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer onv.Disconnect()

	pts := []models.PointDef{{Name: "info", Address: "device_info"}}
	got, err := onv.ReadPoints(ctx, pts)
	if err != nil {
		t.Fatalf("ReadPoints: %v", err)
	}
	if len(got) != 1 || got[0].Quality != "good" {
		t.Fatalf("expected good-quality ONVIF read, got %+v", got)
	}
	if v, ok := got[0].Value.(string); !ok || v != "EdgeLite-Sim" {
		t.Fatalf("manufacturer parse mismatch: %v (%T)", got[0].Value, got[0].Value)
	}
	// The request must have carried a WS-Security UsernameToken with a digest.
	for _, want := range []string{"wsse:Username", "admin", "PasswordDigest", "wsse:Nonce"} {
		if !strings.Contains(gotBody, want) {
			t.Fatalf("SOAP request missing %q; body:\n%s", want, gotBody)
		}
	}
}

// TestONVIFUnknownPointTypeDegradesBad asserts an unmapped point type fails
// gracefully (bad quality, no fabricated value) while the device is reachable.
func TestONVIFUnknownPointTypeDegradesBad(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `<s:Envelope><s:Body><empty/></s:Body></s:Envelope>`)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	host, portStr, _ := net.SplitHostPort(u.Host)
	port, _ := strconv.Atoi(portStr)

	d, err := NewONVIFDriver("onvif-bad", map[string]interface{}{
		"host": host, "port": port, "timeout": 2,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	onv := d.(*ONVIFDriver)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := onv.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer onv.Disconnect()

	pts := []models.PointDef{{Name: "bogus", Address: "totally_unknown_kind"}}
	got, err := onv.ReadPoints(ctx, pts)
	if err != nil {
		t.Fatalf("ReadPoints: %v", err)
	}
	if len(got) != 1 || got[0].Quality != "bad" {
		t.Fatalf("unknown point type should degrade to bad quality, got %+v", got)
	}
	if got[0].Value != nil {
		t.Fatalf("unknown point type must not fabricate a value, got %v", got[0].Value)
	}
}

// TestONVIFUnreachableDeviceStaysOffline pins the removal of the "simulated
// mode" fallback: an unreachable camera must fail Connect, not report itself
// connected so the UI shows a healthy device that can never produce data.
func TestONVIFUnreachableDeviceStaysOffline(t *testing.T) {
	d, err := NewONVIFDriver("onvif-offline", map[string]interface{}{
		"host": "127.0.0.1", "port": 1, "timeout": 1,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	onv := d.(*ONVIFDriver)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := onv.Connect(ctx); err == nil {
		t.Fatal("Connect to a closed port must fail now that simulated mode is gone")
	}
	if onv.IsConnected() {
		t.Fatal("failed Connect must leave the driver disconnected")
	}
	if _, err := onv.ReadPoints(ctx, []models.PointDef{{Name: "info", Address: "device_info"}}); err == nil {
		t.Fatal("ReadPoints on a disconnected driver must fail")
	}
}

// TestONVIFWriteIsRejected pins that ONVIF never claims a write succeeded: the
// driver implements no PTZ or media control commands.
func TestONVIFWriteIsRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `<s:Envelope><s:Body><empty/></s:Body></s:Envelope>`)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	host, portStr, _ := net.SplitHostPort(u.Host)
	port, _ := strconv.Atoi(portStr)

	d, err := NewONVIFDriver("onvif-write", map[string]interface{}{
		"host": host, "port": port, "timeout": 2,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	onv := d.(*ONVIFDriver)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := onv.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer onv.Disconnect()

	if err := onv.WritePoint(ctx, "ptz_move", 1.0); !errors.Is(err, ErrONVIFWriteUnsupported) {
		t.Fatalf("ONVIF write must report the missing capability, got %v", err)
	}
}

// TestComputeSoapDigestKnownVector pins the WS-Security PasswordDigest formula:
// Base64(SHA1(rawNonceBytes + Created + Password)).
func TestComputeSoapDigestKnownVector(t *testing.T) {
	nonceB64 := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	created := "2026-01-01T00:00:00Z"
	password := "secret"

	raw := append([]byte("0123456789abcdef"), []byte(created)...)
	raw = append(raw, []byte(password)...)
	sum := sha1.Sum(raw)
	want := base64.StdEncoding.EncodeToString(sum[:])

	if got := computeSoapDigest(nonceB64, created, password); got != want {
		t.Fatalf("computeSoapDigest = %q, want %q", got, want)
	}
}

// TestParseONVIFResponseHelpers covers the response-shaping helpers used to extract
// values from SOAP replies.
func TestParseONVIFResponseHelpers(t *testing.T) {
	if v := parseONVIFResponse("<tds:Manufacturer>Acme</tds:Manufacturer>", "device_info"); v != "Acme" {
		t.Errorf("device_info parse = %v, want Acme", v)
	}
	if n := parseONVIFResponse("<trt:Profiles x='1'/><trt:Profiles x='2'/>", "video"); n != 2 {
		t.Errorf("video profile count = %v, want 2", n)
	}
	addr, ok := parseONVIFDeviceAddress("<ProbeMatch><XAddrs>http://1.2.3.4/onvif</XAddrs></ProbeMatch>")
	if !ok || addr != "http://1.2.3.4/onvif" {
		t.Errorf("parseONVIFDeviceAddress = (%q,%v)", addr, ok)
	}
}
