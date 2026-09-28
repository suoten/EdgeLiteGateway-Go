package drivers

// TLS coverage for mqtt_client. The driver builds its own tls.Config from the
// device config and dials with it, so every branch below (cert_reqs modes, PEM
// vs file, mTLS) is a path an operator can actually select in the UI. The
// important property is that a verification failure is a hard Connect error:
// falling back to plaintext would leave the credentials on the wire while the
// device still showed "online".

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type testBrokerCA struct {
	caPEM  []byte // the PEM an operator pastes into ca_cert
	caCert *x509.Certificate
	caKey  *ecdsa.PrivateKey
	server tls.Certificate
	pool   *x509.CertPool
}

// newTestBrokerCA mints a throwaway CA plus a server certificate valid for
// 127.0.0.1/localhost. Plant brokers ship exactly this shape (private CA, short
// leaf validity), so the verification paths below are the real ones.
func newTestBrokerCA(t *testing.T) *testBrokerCA {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "edgelite-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA certificate: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse CA certificate: %v", err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})

	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate server key: %v", err)
	}
	serverTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano() + 1),
		Subject:               pkix.Name{CommonName: "edgelite-test-broker"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		BasicConstraintsValid: true,
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTmpl, caCert, &serverKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create server certificate: %v", err)
	}
	serverKeyDER, err := x509.MarshalECPrivateKey(serverKey)
	if err != nil {
		t.Fatalf("marshal server key: %v", err)
	}
	serverPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER})
	serverKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: serverKeyDER})
	pair, err := tls.X509KeyPair(serverPEM, serverKeyPEM)
	if err != nil {
		t.Fatalf("server key pair: %v", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("test CA PEM did not parse")
	}
	return &testBrokerCA{caPEM: caPEM, caCert: caCert, caKey: caKey, server: pair, pool: pool}
}

// clientPEM issues a gateway client certificate from the same CA, which is what
// a mutual-TLS broker validates against.
func (c *testBrokerCA) clientPEM(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate client key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano() + 2),
		Subject:               pkix.Name{CommonName: "edgelite-test-gateway"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.caCert, &key.PublicKey, c.caKey)
	if err != nil {
		t.Fatalf("create client certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal client key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

// newTLSPFMockBroker reuses the packet-level mock broker behind a TLS listener,
// so the driver performs a real handshake before any MQTT byte is sent.
func newTLSPFMockBroker(t *testing.T, srvCfg *tls.Config) (*pfMockBroker, int) {
	t.Helper()
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ln := tls.NewListener(tcp, srvCfg)
	b := &pfMockBroker{ln: ln}
	go b.serve()
	_, portStr, err := net.SplitHostPort(tcp.Addr().String())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	port := 0
	for _, ch := range portStr {
		port = port*10 + int(ch-'0')
	}
	return b, port
}

func (c *testBrokerCA) serverConfig() *tls.Config {
	return &tls.Config{Certificates: []tls.Certificate{c.server}, MinVersion: tls.VersionTLS12}
}

func connectTLSDriver(t *testing.T, port int, cfg map[string]interface{}) (*MQTTClientDriver, error) {
	t.Helper()
	cfg["broker"] = "127.0.0.1"
	cfg["port"] = port
	cfg["timeout"] = 3
	d, err := NewMQTTClientDriver("mqtttls", cfg)
	if err != nil {
		return nil, err
	}
	md := d.(*MQTTClientDriver)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return md, md.Connect(ctx)
}

// TestMQTTClientTLSVerifiesBrokerWithConfiguredCA is the golden path: the
// operator pasted the broker CA, cert_reqs stays "required", and the write has
// to travel over a verified channel.
func TestMQTTClientTLSVerifiesBrokerWithConfiguredCA(t *testing.T) {
	ca := newTestBrokerCA(t)
	broker, port := newTLSPFMockBroker(t, ca.serverConfig())
	defer broker.ln.Close()

	md, err := connectTLSDriver(t, port, map[string]interface{}{
		"tls_enabled":  true,
		"ca_cert":      string(ca.caPEM),
		"topic_prefix": "plant7",
	})
	if err != nil {
		t.Fatalf("connect over TLS with the configured CA: %v", err)
	}
	defer md.Disconnect()

	if md.tlsCfg == nil {
		t.Fatal("tls_enabled must build a tls.Config, got nil (plaintext fallback)")
	}
	if md.tlsCfg.InsecureSkipVerify {
		t.Fatal("cert_reqs=required must not disable verification")
	}
	if md.tlsCfg.RootCAs == nil {
		t.Fatal("ca_cert was not installed as RootCAs")
	}
	broker.mu.Lock()
	seen := broker.connacks
	broker.mu.Unlock()
	if seen != 1 {
		t.Fatalf("broker saw %d CONNACKs over TLS, want 1", seen)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := md.WritePoint(ctx, "setpoint", 42); err != nil {
		t.Fatalf("publish over TLS: %v", err)
	}
	pub := broker.waitPublish(t, 1)
	if want := "plant7/mqtttls/command"; pub.topic != want {
		t.Fatalf("publish topic = %q, want %q (topic_prefix ignored)", pub.topic, want)
	}
	if !strings.Contains(string(pub.payload), "42") {
		t.Fatalf("published payload = %q, want the written value", string(pub.payload))
	}
}

// TestMQTTClientTLSCertificateFileIsLoaded pins the other half of readPEMOrFile:
// the field also accepts a path, which is how a deployed gateway mounts its CA.
func TestMQTTClientTLSCertificateFileIsLoaded(t *testing.T) {
	ca := newTestBrokerCA(t)
	path := filepath.Join(t.TempDir(), "broker-ca.pem")
	if err := os.WriteFile(path, ca.caPEM, 0o600); err != nil {
		t.Fatalf("write CA: %v", err)
	}
	broker, port := newTLSPFMockBroker(t, ca.serverConfig())
	defer broker.ln.Close()

	md, err := connectTLSDriver(t, port, map[string]interface{}{
		"tls_enabled": true,
		"ca_cert":     path,
	})
	if err != nil {
		t.Fatalf("connect with CA file: %v", err)
	}
	md.Disconnect()
}

// TestMQTTClientTLSRejectsUntrustedBroker is the honesty guard: with no CA that
// trusts the broker the connection must fail, not silently continue in
// plaintext or with verification switched off.
func TestMQTTClientTLSRejectsUntrustedBroker(t *testing.T) {
	ca := newTestBrokerCA(t)
	broker, port := newTLSPFMockBroker(t, ca.serverConfig())
	defer broker.ln.Close()

	md, err := connectTLSDriver(t, port, map[string]interface{}{"tls_enabled": true})
	if err == nil {
		md.Disconnect()
		t.Fatal("Connect accepted an untrusted broker certificate")
	}
	if md.IsConnected() {
		t.Fatal("failed handshake must leave the driver disconnected")
	}
	if !strings.Contains(err.Error(), "certificate") && !strings.Contains(err.Error(), "x509") {
		t.Fatalf("error = %v, want a certificate verification failure", err)
	}
}

// TestMQTTClientTLSCertReqsNonePinsTheExplicitEscapeHatch: "none" is the only
// mode allowed to skip verification, and it still has to be real TLS.
func TestMQTTClientTLSCertReqsNonePinsTheExplicitEscapeHatch(t *testing.T) {
	ca := newTestBrokerCA(t)
	broker, port := newTLSPFMockBroker(t, ca.serverConfig())
	defer broker.ln.Close()

	md, err := connectTLSDriver(t, port, map[string]interface{}{
		"tls_enabled": true,
		"cert_reqs":   "none",
	})
	if err != nil {
		t.Fatalf("cert_reqs=none should connect to a self-signed broker: %v", err)
	}
	defer md.Disconnect()
	if !md.tlsCfg.InsecureSkipVerify {
		t.Fatal("cert_reqs=none must set InsecureSkipVerify")
	}
	if md.tlsCfg.RootCAs != nil || len(md.tlsCfg.Certificates) != 0 {
		t.Fatal("cert_reqs=none must not invent CA or client material")
	}
}

// TestMQTTClientTLSOptionalModeUsesSystemPool: "optional" with a CA is a normal
// verified connection; "optional" without one behaves like "none".
func TestMQTTClientTLSOptionalModeUsesSystemPool(t *testing.T) {
	ca := newTestBrokerCA(t)
	broker, port := newTLSPFMockBroker(t, ca.serverConfig())
	defer broker.ln.Close()

	withCA, err := connectTLSDriver(t, port, map[string]interface{}{
		"tls_enabled": true, "cert_reqs": "optional", "ca_cert": string(ca.caPEM),
	})
	if err != nil {
		t.Fatalf("optional with CA should verify: %v", err)
	}
	withCA.Disconnect()
	if withCA.tlsCfg.InsecureSkipVerify {
		t.Fatal("optional with a CA must still verify the broker")
	}

	broker2, port2 := newTLSPFMockBroker(t, ca.serverConfig())
	defer broker2.ln.Close()
	withoutCA, err := connectTLSDriver(t, port2, map[string]interface{}{
		"tls_enabled": true, "cert_reqs": "optional",
	})
	if err != nil {
		t.Fatalf("optional without CA should connect: %v", err)
	}
	withoutCA.Disconnect()
	if !withoutCA.tlsCfg.InsecureSkipVerify {
		t.Fatal("optional without a CA has nothing to verify against, expected skip")
	}
}

// TestMQTTClientMTLSPresentsClientCert covers the mutual-TLS branch: the server
// demands a certificate, so a successful CONNACK is only possible if the driver
// actually sent one.
func TestMQTTClientMTLSPresentsClientCert(t *testing.T) {
	ca := newTestBrokerCA(t)
	clientCertPEM, clientKeyPEM := ca.clientPEM(t)

	srvCfg := ca.serverConfig()
	srvCfg.ClientAuth = tls.RequireAndVerifyClientCert
	srvCfg.ClientCAs = ca.pool

	broker, port := newTLSPFMockBroker(t, srvCfg)
	defer broker.ln.Close()

	md, err := connectTLSDriver(t, port, map[string]interface{}{
		"tls_enabled": true,
		"ca_cert":     string(ca.caPEM),
		"client_cert": string(clientCertPEM),
		"client_key":  string(clientKeyPEM),
	})
	if err != nil {
		t.Fatalf("mTLS connect: %v", err)
	}
	defer md.Disconnect()
	if len(md.tlsCfg.Certificates) != 1 {
		t.Fatalf("driver offered %d client certificates, want 1", len(md.tlsCfg.Certificates))
	}
	broker.mu.Lock()
	seen := broker.connacks
	broker.mu.Unlock()
	if seen != 1 {
		t.Fatalf("broker accepted %d CONNECTs, want 1", seen)
	}

	// The same server must reject a driver that brings no client certificate,
	// otherwise the assertion above could be satisfied by a handshake that never
	// asked for one.
	broker2, port2 := newTLSPFMockBroker(t, srvCfg)
	defer broker2.ln.Close()
	if _, err := connectTLSDriver(t, port2, map[string]interface{}{
		"tls_enabled": true, "ca_cert": string(ca.caPEM),
	}); err == nil {
		t.Fatal("mTLS broker accepted a driver without a client certificate")
	}
}

// TestMQTTClientTLSBadConfigFailsAtConstruction: a broken certificate must be a
// device-creation error the operator sees, not a connection that degrades later.
func TestMQTTClientTLSBadConfigFailsAtConstruction(t *testing.T) {
	ca := newTestBrokerCA(t)
	dir := t.TempDir()
	notPEM := filepath.Join(dir, "noise.pem")
	if err := os.WriteFile(notPEM, []byte("not a certificate"), 0o600); err != nil {
		t.Fatalf("write noise: %v", err)
	}
	clientCertPEM, clientKeyPEM := ca.clientPEM(t)

	cases := []struct {
		name   string
		config map[string]interface{}
		want   string
	}{
		{
			name:   "unknown cert_reqs",
			config: map[string]interface{}{"tls_enabled": true, "cert_reqs": "when-you-feel-it"},
			want:   "invalid cert_reqs",
		},
		{
			name:   "ca_cert is neither PEM nor a file",
			config: map[string]interface{}{"tls_enabled": true, "ca_cert": filepath.Join(dir, "missing.pem")},
			want:   "ca_cert",
		},
		{
			name:   "ca_cert file has no certificate",
			config: map[string]interface{}{"tls_enabled": true, "ca_cert": notPEM},
			want:   "no parseable PEM",
		},
		{
			name:   "client key without certificate",
			config: map[string]interface{}{"tls_enabled": true, "client_key": string(clientKeyPEM)},
			want:   "must be configured together",
		},
		{
			name:   "client certificate without key",
			config: map[string]interface{}{"tls_enabled": true, "client_cert": string(clientCertPEM)},
			want:   "must be configured together",
		},
		{
			name: "mismatched client pair",
			config: map[string]interface{}{
				"tls_enabled": true, "client_cert": string(clientCertPEM), "client_key": string(ca.caPEM),
			},
			want: "client certificate/key pair",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.config["broker"] = "127.0.0.1"
			tc.config["port"] = 1883
			if _, err := NewMQTTClientDriver("bad", tc.config); err == nil {
				t.Fatalf("expected construction to fail with %q", tc.want)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestMQTTClientTLSDisabledStaysPlaintext keeps the negative case honest too:
// without tls_enabled the driver must not wrap the socket.
func TestMQTTClientTLSDisabledStaysPlaintext(t *testing.T) {
	broker, port := newPFMockBroker(t)
	defer broker.ln.Close()

	md, err := connectTLSDriver(t, port, map[string]interface{}{"tls_enabled": false})
	if err != nil {
		t.Fatalf("plaintext connect: %v", err)
	}
	defer md.Disconnect()
	if md.tlsCfg != nil {
		t.Fatal("tls_enabled=false must not build a tls.Config")
	}
}
