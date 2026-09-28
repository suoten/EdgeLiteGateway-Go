package api

// Regression tests for the credential round-trip on the MQTT pages and for
// where PUT /system/config persists. Both bugs destroyed real state: the UI
// re-sent the masked password it had been shown, which was stored verbatim so
// the next broker connection authenticated as "b***1", and a gateway started
// with --config wrote UI edits into the packaged configs/config.yaml that it
// never reads.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"edgelite/internal/config"
)

// isolatedTestSecretKey keeps the throwaway config loadable: LoadConfig rejects
// an empty or short security.secret_key, and the in-memory config a test starts
// from is not necessarily the one on disk.
const isolatedTestSecretKey = "isolated-test-secret-key-0123456789abcdef-32chars"

// writeIsolatedConfig seeds a per-test copy of the shipped config and returns its
// path. A single file shared by the whole package cannot work: every handler
// under test writes the live global config over it, so one test that starts from
// a zero-valued config leaves a file the next test can no longer load.
func writeIsolatedConfig(t *testing.T) string {
	t.Helper()
	seed := []byte("security:\n    secret_key: " + isolatedTestSecretKey + "\n")
	if src, path := shippedConfig(t); path != "" {
		seed = src
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, seed, 0600); err != nil {
		t.Fatalf("seed isolated config: %v", err)
	}
	return path
}

// withIsolatedConfig points the global config at a throwaway file and restores
// the previous state when the test ends: these handlers mutate the global
// instance and SaveConfig writes it over, so every assertion about what survives
// a restart has to read back the file this test alone owns.
func withIsolatedConfig(t *testing.T) {
	t.Helper()
	prev := config.GetConfig()
	prevContainer := GetContainer()
	// An empty container leaves MqttServer/MqttForward nil, so the handlers take
	// the "nothing running" branch instead of binding a real port in a test.
	SetContainer(NewServiceContainer())
	path := writeIsolatedConfig(t)
	if _, err := config.LoadConfig(path); err != nil {
		t.Fatalf("load isolated config %s: %v", path, err)
	}
	t.Cleanup(func() {
		SetContainer(prevContainer)
		config.SetGlobalConfig(prev)
	})
}

// persistedConfig reloads what the instance actually wrote to disk, which is
// the only copy that survives a restart.
func persistedConfig(t *testing.T) *config.AppConfig {
	t.Helper()
	path := config.LoadedConfigPath()
	if path == "" {
		path = os.Getenv("EDGELITE_CONFIG")
	}
	if path == "" {
		t.Fatal("no config path is in use, isolation is broken")
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("reload persisted config: %v", err)
	}
	return cfg
}

func TestUpdateMQTTServerConfigPreservesMaskedPassword(t *testing.T) {
	withIsolatedConfig(t)
	cfg := config.GetConfig()
	cfg.MqttServer = config.MqttServerConfig{
		Enabled: true, Host: "127.0.0.1", Port: 1888,
		Username: "gateway", Password: "broker-secret-1",
	}

	// What MqttServer.vue sends after the user only changes the port: the
	// credentials come straight back as the masks from GET /config.
	c, rec := setupWithAdmin(echo.PUT, "/api/v1/mqtt-server/config",
		`{"enabled":true,"host":"0.0.0.0","port":1890,"username":"g***y","password":"b***1"}`)
	if err := handleUpdateMQTTServerConfig(c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "broker-secret-1") {
		t.Fatal("response leaked the plaintext broker password")
	}

	saved := persistedConfig(t)
	if saved.MqttServer.Password != "broker-secret-1" {
		t.Fatalf("masked password was persisted: %q", saved.MqttServer.Password)
	}
	if saved.MqttServer.Username != "gateway" {
		t.Fatalf("masked username was persisted: %q", saved.MqttServer.Username)
	}
	// The change the operator did make has to land, or the restore is hiding it.
	if saved.MqttServer.Port != 1890 || saved.MqttServer.Host != "0.0.0.0" {
		t.Fatalf("port/host edit lost: %+v", saved.MqttServer)
	}
}

func TestUpdateMQTTForwarderConfigPreservesMaskedPassword(t *testing.T) {
	withIsolatedConfig(t)
	cfg := config.GetConfig()
	cfg.MQTT = config.MQTTConfig{
		Broker: "10.0.0.5", Port: 1883, Username: "forwarder",
		Password: "north-secret-9", TopicPrefix: "edgelite", MaxQueueSize: 1000,
	}

	c, rec := setupWithAdmin(echo.PUT, "/api/v1/mqtt-forwarder/config",
		`{"broker":"10.0.0.9","port":1883,"username":"f***r","password":"n***9","topic_prefix":"edgelite","max_queue_size":2000}`)
	if err := handleUpdateMQTTForwarderConfig(c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "north-secret-9") {
		t.Fatal("response leaked the plaintext broker password")
	}

	saved := persistedConfig(t)
	if saved.MQTT.Password != "north-secret-9" {
		t.Fatalf("masked password was persisted: %q", saved.MQTT.Password)
	}
	if saved.MQTT.Username != "forwarder" {
		t.Fatalf("masked username was persisted: %q", saved.MQTT.Username)
	}
	if saved.MQTT.Broker != "10.0.0.9" || saved.MQTT.MaxQueueSize != 2000 {
		t.Fatalf("forwarder edits lost: %+v", saved.MQTT)
	}
}

func TestGetMQTTForwarderConfigMasksPassword(t *testing.T) {
	withIsolatedConfig(t)
	cfg := config.GetConfig()
	cfg.MQTT.Password = "plain-leak-1234"

	c, rec := setupWithAdmin(echo.GET, "/api/v1/mqtt-forwarder/config", "")
	if err := handleGetMQTTForwarderConfig(c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if strings.Contains(rec.Body.String(), "plain-leak-1234") {
		t.Fatalf("GET /mqtt-forwarder/config returned the plaintext password: %s", rec.Body.String())
	}
}

func TestUpdateConfigWritesTheConfigFileThisInstanceLoaded(t *testing.T) {
	withIsolatedConfig(t)
	isolated := config.LoadedConfigPath()
	if isolated == "" {
		t.Fatal("tests are not isolated from the shipped config")
	}
	src, err := os.ReadFile(isolated)
	if err != nil {
		t.Fatalf("read isolated config: %v", err)
	}
	dir := t.TempDir()
	// Stand in for a deployment booted with --config /etc/edgelite/config.yaml.
	outside := filepath.Join(dir, "deploy", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(outside), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(outside, src, 0600); err != nil {
		t.Fatalf("seed deploy config: %v", err)
	}
	if _, err := config.LoadConfig(outside); err != nil {
		t.Fatalf("load deploy config: %v", err)
	}
	t.Cleanup(func() {
		if _, err := config.LoadConfig(isolated); err != nil {
			t.Fatalf("restore isolated config: %v", err)
		}
	})

	c, rec := setupWithAdmin(echo.PUT, "/api/v1/system/config", `{"mqtt_server":{"port":4321}}`)
	if err := handleUpdateConfig(c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	saved, err := config.LoadConfig(outside)
	if err != nil {
		t.Fatalf("reload deploy config: %v", err)
	}
	if saved.MqttServer.Port != 4321 {
		t.Fatalf("config was not written to the loaded path: port=%d", saved.MqttServer.Port)
	}
}

func TestUpdateConfigStillRejectsTraversalOutsideOwnedPaths(t *testing.T) {
	withIsolatedConfig(t)
	target := filepath.Join(t.TempDir(), "escaped.yaml")
	body := `{"mqtt_server":{"port":4321}}`

	for _, path := range []string{"../evil.yaml", target} {
		c, rec := setupWithAdmin(echo.PUT, "/api/v1/system/config?path="+path, body)
		if err := handleUpdateConfig(c); err != nil {
			t.Fatalf("handler error for %q: %v", path, err)
		}
		if rec.Code == 200 {
			t.Fatalf("path %q was accepted for writing", path)
		}
		if rec.Code != 400 {
			t.Fatalf("path %q: expected 400, got %d: %s", path, rec.Code, rec.Body.String())
		}
		if _, err := os.Stat(target); err == nil {
			t.Fatalf("handler wrote %q after rejecting it", target)
		}
	}
}

func putSection(t *testing.T, section, body string) (int, string) {
	t.Helper()
	c, rec := setupWithAdmin(echo.PUT, "/api/v1/system/config/"+section, body)
	c.SetParamNames("section")
	c.SetParamValues(section)
	if err := handleUpdateConfigSection(c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	return rec.Code, rec.Body.String()
}

func TestUpdateConfigSectionPersistsAndMerges(t *testing.T) {
	withIsolatedConfig(t)
	config.GetConfig().MQTT = config.MQTTConfig{
		Broker: "10.1.1.1", Port: 1883, Password: "keep-secret-7", MaxQueueSize: 10,
	}

	code, body := putSection(t, "mqtt", `{"max_retries":3}`)
	if code != 200 {
		t.Fatalf("expected 200, got %d: %s", code, body)
	}
	saved := persistedConfig(t)
	if saved.MQTT.MaxRetries != 3 {
		t.Fatalf("patch not persisted: %+v", saved.MQTT)
	}
	// A section patch must merge, not replace: the broker address and the
	// credential the page never sent have to survive.
	if saved.MQTT.Broker != "10.1.1.1" || saved.MQTT.Password != "keep-secret-7" || saved.MQTT.MaxQueueSize != 10 {
		t.Fatalf("section patch wiped the rest of the section: %+v", saved.MQTT)
	}
}

func TestUpdateConfigSectionKeepsMaskedSecret(t *testing.T) {
	withIsolatedConfig(t)
	config.GetConfig().MQTT = config.MQTTConfig{Broker: "10.1.1.1", Password: "real-secret-3"}

	if code, body := putSection(t, "mqtt", `{"password":"r***3","broker":"10.1.1.2"}`); code != 200 {
		t.Fatalf("expected 200, got %d: %s", code, body)
	}
	saved := persistedConfig(t)
	if saved.MQTT.Password != "real-secret-3" {
		t.Fatalf("mask stored as the password: %q", saved.MQTT.Password)
	}
	if saved.MQTT.Broker != "10.1.1.2" {
		t.Fatalf("broker change lost: %q", saved.MQTT.Broker)
	}
}

func TestUpdateConfigSectionRejectsUnknownSection(t *testing.T) {
	withIsolatedConfig(t)
	before := persistedConfig(t)
	code, body := putSection(t, "nonsense_section", `{"a":1}`)
	if code != 400 {
		t.Fatalf("unknown section accepted: %d %s", code, body)
	}
	if !strings.Contains(body, "nonsense_section") {
		t.Fatalf("rejection should name the section: %s", body)
	}
	if after := persistedConfig(t); after.MQTT.Broker != before.MQTT.Broker {
		t.Fatal("rejected request still changed the config")
	}
}

func TestUpdateConfigSectionReportsDroppedKeys(t *testing.T) {
	withIsolatedConfig(t)
	code, body := putSection(t, "mqtt", `{"max_retries":2,"not_a_real_setting":1}`)
	if code != 200 {
		t.Fatalf("expected 200, got %d: %s", code, body)
	}
	var env struct {
		Data struct {
			UpdatedKeys []string `json:"updated_keys"`
			IgnoredKeys []string `json:"ignored_keys"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode response: %v (%s)", err, body)
	}
	if len(env.Data.IgnoredKeys) != 1 || env.Data.IgnoredKeys[0] != "not_a_real_setting" {
		t.Fatalf("unknown key was not reported, response said success blindly: %s", body)
	}
	if len(env.Data.UpdatedKeys) != 1 || env.Data.UpdatedKeys[0] != "max_retries" {
		t.Fatalf("updated_keys wrong: %v", env.Data.UpdatedKeys)
	}
}
