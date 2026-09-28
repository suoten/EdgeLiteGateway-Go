package engine

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// CertManager manages TLS certificates for devices and services.
type CertManager struct {
	mu      sync.Mutex
	certDir string
	caCert  *x509.Certificate
	caKey   *rsa.PrivateKey
}

// NewCertManager creates a new CertManager.
func NewCertManager(certDir string) *CertManager {
	if certDir == "" {
		certDir = "data/certs"
	}
	return &CertManager{
		certDir: certDir,
	}
}

// GetCACertPath returns the path to the CA certificate.
func (m *CertManager) GetCACertPath() string {
	return filepath.Join(m.certDir, "ca.crt")
}

// GetCAKeyPath returns the path to the CA private key.
func (m *CertManager) GetCAKeyPath() string {
	return filepath.Join(m.certDir, "ca.key")
}

// GetDeviceCertPath returns the path to a device certificate.
func (m *CertManager) GetDeviceCertPath(deviceID string) string {
	return filepath.Join(m.certDir, deviceID+".crt")
}

// GetDeviceKeyPath returns the path to a device private key.
func (m *CertManager) GetDeviceKeyPath(deviceID string) string {
	return filepath.Join(m.certDir, deviceID+".key")
}

// IsCAExists returns whether the CA certificate exists.
func (m *CertManager) IsCAExists() bool {
	_, err := os.Stat(m.GetCACertPath())
	return err == nil
}

// LoadCert loads a certificate from a file.
func (m *CertManager) LoadCert(certPath string) (string, error) {
	data, err := os.ReadFile(certPath)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// SaveCert saves certificate content to a file.
func (m *CertManager) SaveCert(certPath, content string) error {
	if err := os.MkdirAll(filepath.Dir(certPath), 0755); err != nil {
		return err
	}
	return os.WriteFile(certPath, []byte(content), 0644)
}

// GetCertFingerprint returns the SHA256 fingerprint of a certificate.
func (m *CertManager) GetCertFingerprint(certContent string) string {
	block, _ := pem.Decode([]byte(certContent))
	if block == nil {
		return ""
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%X", cert.Signature)
}

// ValidateCert validates a certificate file.
func (m *CertManager) ValidateCert(certPath string) bool {
	data, err := os.ReadFile(certPath)
	if err != nil {
		return false
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}
	// Check not expired
	now := time.Now()
	if now.Before(cert.NotBefore) || now.After(cert.NotAfter) {
		return false
	}
	return true
}

// GenerateCA generates a self-signed CA certificate.
func (m *CertManager) GenerateCA() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.IsCAExists() {
		// Load existing CA
		return m.loadCA()
	}

	// Generate CA private key
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return fmt.Errorf("failed to generate CA key: %w", err)
	}

	// Create CA certificate template
	caTemplate := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"EdgeLite Gateway"},
			CommonName:   "EdgeLite CA",
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	// Self-sign the CA certificate
	caCertBytes, err := x509.CreateCertificate(rand.Reader, &caTemplate, &caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return fmt.Errorf("failed to create CA certificate: %w", err)
	}

	// Save CA certificate
	caCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caCertBytes})
	if err := os.MkdirAll(m.certDir, 0755); err != nil {
		return err
	}
	if err := os.WriteFile(m.GetCACertPath(), caCertPEM, 0644); err != nil {
		return err
	}

	// Save CA private key
	caKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(caKey)})
	if err := os.WriteFile(m.GetCAKeyPath(), caKeyPEM, 0600); err != nil {
		return err
	}

	// Parse and store the CA cert
	caCert, err := x509.ParseCertificate(caCertBytes)
	if err != nil {
		return err
	}
	m.caCert = caCert
	m.caKey = caKey

	return nil
}

// GenerateDeviceCert generates a certificate for a device signed by the CA.
func (m *CertManager) GenerateDeviceCert(deviceID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.caCert == nil || m.caKey == nil {
		if err := m.loadCA(); err != nil {
			return fmt.Errorf("CA not available: %w", err)
		}
	}

	// Generate device key
	deviceKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("failed to generate device key: %w", err)
	}

	// Create device certificate template
	deviceTemplate := x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject: pkix.Name{
			Organization: []string{"EdgeLite Gateway"},
			CommonName:   deviceID,
		},
		NotBefore:   time.Now(),
		NotAfter:    time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	}

	// Sign with CA
	deviceCertBytes, err := x509.CreateCertificate(rand.Reader, &deviceTemplate, m.caCert, &deviceKey.PublicKey, m.caKey)
	if err != nil {
		return fmt.Errorf("failed to create device certificate: %w", err)
	}

	// Save device certificate
	deviceCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: deviceCertBytes})
	if err := os.WriteFile(m.GetDeviceCertPath(deviceID), deviceCertPEM, 0644); err != nil {
		return err
	}

	// Save device private key
	deviceKeyBytes, err := x509.MarshalECPrivateKey(deviceKey)
	if err != nil {
		return err
	}
	deviceKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: deviceKeyBytes})
	if err := os.WriteFile(m.GetDeviceKeyPath(deviceID), deviceKeyPEM, 0600); err != nil {
		return err
	}

	return nil
}

// loadCA loads the CA certificate and key from disk.
func (m *CertManager) loadCA() error {
	caCertPEM, err := os.ReadFile(m.GetCACertPath())
	if err != nil {
		return err
	}
	block, _ := pem.Decode(caCertPEM)
	if block == nil {
		return fmt.Errorf("failed to decode CA cert PEM")
	}
	caCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return err
	}

	caKeyPEM, err := os.ReadFile(m.GetCAKeyPath())
	if err != nil {
		return err
	}
	keyBlock, _ := pem.Decode(caKeyPEM)
	if keyBlock == nil {
		return fmt.Errorf("failed to decode CA key PEM")
	}
	caKey, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes)
	if err != nil {
		return err
	}

	m.caCert = caCert
	m.caKey = caKey
	return nil
}

// TlsConfigBuilder builds TLS configurations for different services.
type TlsConfigBuilder struct {
	certManager *CertManager
}

// NewTlsConfigBuilder creates a new TlsConfigBuilder.
func NewTlsConfigBuilder(certManager *CertManager) *TlsConfigBuilder {
	return &TlsConfigBuilder{certManager: certManager}
}

// BuildMqttTLSConfig builds a TLS configuration for MQTT.
func (b *TlsConfigBuilder) BuildMqttTLSConfig(certFile, keyFile, caFile string, insecureSkipVerify bool) (*tls.Config, error) {
	var cert tls.Certificate
	var err error
	if certFile != "" && keyFile != "" {
		cert, err = tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("failed to load MQTT cert: %w", err)
		}
	}

	tlsConfig := &tls.Config{
		Certificates:           []tls.Certificate{cert},
		InsecureSkipVerify:     insecureSkipVerify,
		MinVersion:             tls.VersionTLS12,
	}

	if caFile != "" {
		caCert, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read CA cert: %w", err)
		}
		caPool := x509.NewCertPool()
		if !caPool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("failed to append CA cert")
		}
		tlsConfig.RootCAs = caPool
	}

	return tlsConfig, nil
}

// BuildHttpsTLSConfig builds a TLS configuration for HTTPS.
func (b *TlsConfigBuilder) BuildHttpsTLSConfig(certFile, keyFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load HTTPS cert: %w", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// TlsManager provides high-level TLS management.
type TlsManager struct {
	certManager *CertManager
	builder     *TlsConfigBuilder
}

// NewTlsManager creates a new TlsManager.
func NewTlsManager(certDir string) *TlsManager {
	cm := NewCertManager(certDir)
	return &TlsManager{
		certManager: cm,
		builder:     NewTlsConfigBuilder(cm),
	}
}

// SetupMqttTLS sets up TLS for MQTT.
func (m *TlsManager) SetupMqttTLS(certFile, keyFile, caFile string, insecure bool) (*tls.Config, error) {
	return m.builder.BuildMqttTLSConfig(certFile, keyFile, caFile, insecure)
}

// SetupOpcuaTLS sets up TLS for OPC UA.
func (m *TlsManager) SetupOpcuaTLS(certFile, keyFile, caFile string, serverName string) (*tls.Config, error) {
	tlsConfig, err := m.builder.BuildMqttTLSConfig(certFile, keyFile, caFile, false)
	if err != nil {
		return nil, err
	}
	if serverName != "" {
		tlsConfig.ServerName = serverName
	}
	return tlsConfig, nil
}

// GetCertFingerprint returns the fingerprint of a certificate file.
func (m *TlsManager) GetCertFingerprint(certPath string) (string, error) {
	content, err := m.certManager.LoadCert(certPath)
	if err != nil {
		return "", err
	}
	fp := m.certManager.GetCertFingerprint(content)
	if fp == "" {
		return "", fmt.Errorf("failed to compute fingerprint")
	}
	return fp, nil
}

// HashFingerprint returns the SHA256 hash of a string.
func HashFingerprint(s string) string {
	h := sha256.Sum256([]byte(s))
	return strings.ToUpper(fmt.Sprintf("%x", h[:]))
}
