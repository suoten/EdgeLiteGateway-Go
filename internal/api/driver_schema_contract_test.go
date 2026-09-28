package api

// The driver config schema endpoint is what the "driver" page shows as the
// authoritative list of knobs a protocol supports. A field that no driver
// actually reads is worse than a missing field: the operator sets it, the API
// accepts it, and nothing changes. These tests bind the declared schema to the
// GetConfig* calls in internal/drivers so a phantom field fails the build.

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var (
	configKeyRe  = regexp.MustCompile(`GetConfig(?:String|Int|Bool|Float)\(\s*[^,]+,\s*"([a-z0-9_]+)"`)
	configIdxRe  = regexp.MustCompile(`config\["([a-z0-9_]+)"\]`)
	driverNameRe = regexp.MustCompile(`func \([\w ]+\*?\w+\) Name\(\) string \{\s*return "([a-z0-9_]+)"`)
)

// driverSourceRoot resolves internal/drivers from the package directory, with a
// couple of fallbacks for `go test ./...` invoked from the module root.
func driverSourceRoot(t *testing.T) string {
	t.Helper()
	for _, candidate := range []string{
		filepath.Join("..", "drivers"),
		filepath.Join("internal", "drivers"),
		filepath.Join("..", "..", "internal", "drivers"),
	} {
		if st, err := os.Stat(candidate); err == nil && st.IsDir() {
			return candidate
		}
	}
	t.Fatal("cannot locate internal/drivers to read the driver sources")
	return ""
}

// protocolConfigKeys maps each driver's Name() to the config keys its source
// reads. Several drivers live in one file, so the key set is per file: it is
// deliberately generous, which only makes the phantom-field check weaker in the
// safe direction.
func protocolConfigKeys(t *testing.T) map[string]map[string]bool {
	t.Helper()
	root := driverSourceRoot(t)
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read %s: %v", root, err)
	}
	perFile := map[string]map[string]bool{}
	byProtocol := map[string]map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		src := string(data)
		keys := map[string]bool{}
		for _, m := range configKeyRe.FindAllStringSubmatch(src, -1) {
			keys[m[1]] = true
		}
		for _, m := range configIdxRe.FindAllStringSubmatch(src, -1) {
			keys[m[1]] = true
		}
		perFile[e.Name()] = keys
		for _, m := range driverNameRe.FindAllStringSubmatch(src, -1) {
			byProtocol[m[1]] = keys
		}
	}
	return byProtocol
}

func TestDriverSchemaFieldsAreActuallyReadByDrivers(t *testing.T) {
	read := protocolConfigKeys(t)

	// Keys the device layer or the API consumes on behalf of the driver, so they
	// belong in a schema even though no GetConfig* call names them.
	shared := map[string]bool{
		"write_audit":     true, // internal/api/devices.go suppresses the audit row
		"update_interval": true, // simulator cadence is engine-side
	}

	for protocol, body := range driverConfigSchemas {
		keys, ok := read[protocol]
		if !ok {
			t.Errorf("driverConfigSchemas documents %q but no driver reports that Name()", protocol)
			continue
		}
		for _, field := range body.Fields {
			if keys[field.Name] || shared[field.Name] {
				continue
			}
			t.Errorf("schema %s advertises %q but no driver reads it (phantom field)", protocol, field.Name)
		}
	}
}

func TestEveryRegisteredDriverHasASchema(t *testing.T) {
	read := protocolConfigKeys(t)
	for protocol := range read {
		if _, ok := driverConfigSchemas[protocol]; !ok {
			t.Errorf("driver %q is registered but has no config schema; the driver page cannot show its fields", protocol)
		}
	}
}

func TestDriverSchemaFieldTypesAreUsable(t *testing.T) {
	knownTypes := map[string]bool{
		"string": true, "integer": true, "number": true, "boolean": true, "password": true,
	}
	for protocol, body := range driverConfigSchemas {
		if strings.TrimSpace(body.Description) == "" {
			t.Errorf("schema %s has no description", protocol)
		}
		seen := map[string]bool{}
		for _, field := range body.Fields {
			if !knownTypes[field.Type] {
				t.Errorf("schema %s field %s has type %q, which the config form cannot render", protocol, field.Name, field.Type)
			}
			if strings.TrimSpace(field.Label) == "" {
				t.Errorf("schema %s field %s has no label", protocol, field.Name)
			}
			if seen[field.Name] {
				t.Errorf("schema %s declares %s twice", protocol, field.Name)
			}
			seen[field.Name] = true
		}
	}
}

// The schema is served as JSON, so a field that drops out of the encoding would
// only show up as a shorter form than the driver supports.
func TestDriverSchemaEndpointsEncodeEveryProtocol(t *testing.T) {
	for protocol := range driverConfigSchemas {
		name := protocol
		t.Run(name, func(t *testing.T) {
			c, rec := setupWithAdmin("GET", "/api/v1/drivers/"+name+"/config-schema", "")
			c.SetParamNames("driver_name")
			c.SetParamValues(name)
			if err := handleGetDriverConfigSchema(c); err != nil {
				t.Fatalf("handleGetDriverConfigSchema: %v", err)
			}
			if rec.Code != 200 {
				t.Fatalf("status %d, want 200 (schema for a registered driver must resolve)", rec.Code)
			}
			body := rec.Body.String()
			if !strings.Contains(body, `"fields"`) {
				t.Fatalf("response has no fields list: %s", body)
			}
			for _, field := range driverConfigSchemas[name].Fields {
				if !strings.Contains(body, strconv.Quote(field.Name)) {
					t.Errorf("field %q of %s missing from the JSON response", field.Name, name)
				}
			}
		})
	}
}
