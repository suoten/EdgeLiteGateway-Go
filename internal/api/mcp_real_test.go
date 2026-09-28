package api

// /mcp/call used to answer {"result":{}} for any tool name and /mcp/tools listed
// nothing, because services.MCPService - a working registry - was never given any
// tools outside its own package tests. The page therefore showed an MCP server with
// no tools, no resources, no prompts and no API keys, while POST /auth-keys still
// returned 201 and GET /sse still opened a stream that never sent an event.
//
// These tests pin the replacements: the six read-only tools have to return what is
// actually in the store, and the two capabilities this build does not have (API
// keys, an SSE transport) have to refuse by code instead of answering 200.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"edgelite/internal/config"
	"edgelite/internal/engine"
	"edgelite/internal/models"
	"edgelite/internal/services"
	"edgelite/internal/storage"
)

// withMCPStore installs a container whose repositories point at a throwaway
// database, then registers the tools the way main does. Registering through the
// real entry point is the point: a test that built its own tool list could pass
// while main never called it, which is exactly how the empty registry survived.
func withMCPStore(t *testing.T) *ServiceContainer {
	t.Helper()
	prev := GetContainer()
	SetContainer(NewServiceContainer())
	cont := GetContainer()

	dir := t.TempDir()
	cfg := &config.AppConfig{}
	cfg.Database.SQLitePath = filepath.Join(dir, "main.db")
	cfg.InfluxDB.SQLiteTSPath = filepath.Join(dir, "ts.db")

	db, err := storage.NewDatabase(cfg)
	if err != nil {
		t.Fatalf("NewDatabase: %v", err)
	}
	tsDB, err := storage.NewTimeSeriesStorage(cfg)
	if err != nil {
		db.Close()
		t.Fatalf("NewTimeSeriesStorage: %v", err)
	}
	cont.Database = db
	cont.DeviceRepo = storage.NewDeviceRepo(db)
	cont.RuleRepo = storage.NewRuleRepo(db)
	cont.AlarmRepo = storage.NewAlarmRepo(db)
	cont.TsStorage = tsDB
	cont.MCPService = services.NewMCPService()
	cont.SystemService = services.NewSystemService(cfg)
	RegisterMCPTools(cont)
	// The routes refuse while mcp_server is switched off (mcpServiceEnabled), so a
	// store under test has to be enabled the way the console does. This is the
	// serving path; TestMCPServerSwitchGatesTheToolSurface covers the off path.
	setMCPServiceEnabled(true)

	t.Cleanup(func() {
		SetContainer(prev)
		_ = tsDB.Close()
		_ = db.Close()
	})
	return cont
}

func callMCP(t *testing.T, h func(echo.Context) error, method, path, body string, params ...string) (int, json.RawMessage, string) {
	t.Helper()
	e := echo.New()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if len(params)%2 != 0 {
		t.Fatalf("params must come in name/value pairs: %#v", params)
	}
	if len(params) > 0 {
		names := make([]string, 0, len(params)/2)
		values := make([]string, 0, len(params)/2)
		for i := 0; i < len(params); i += 2 {
			names = append(names, params[i])
			values = append(values, params[i+1])
		}
		c.SetParamNames(names...)
		c.SetParamValues(values...)
	}
	if err := h(c); err != nil {
		t.Fatalf("handler returned an error: %v", err)
	}
	var env struct {
		ErrorCode string          `json:"error_code"`
		Data      json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unparseable envelope %s: %v", rec.Body, err)
	}
	return rec.Code, env.Data, env.ErrorCode
}

// mcpMap decodes an object payload. A handler that answers an array is a contract
// change, so failing loudly here is the point.
func mcpMap(t *testing.T, raw json.RawMessage) map[string]interface{} {
	t.Helper()
	out := map[string]interface{}{}
	if len(raw) == 0 {
		return out
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("payload is not an object: %s", raw)
	}
	return out
}

func mcpList(t *testing.T, raw json.RawMessage) []map[string]interface{} {
	t.Helper()
	out := []map[string]interface{}{}
	if len(raw) == 0 {
		return out
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("payload is not a list of objects: %s", raw)
	}
	return out
}

func mcpRows(raw interface{}) []map[string]interface{} {
	list, _ := raw.([]interface{})
	out := make([]map[string]interface{}, 0, len(list))
	for _, item := range list {
		if row, ok := item.(map[string]interface{}); ok {
			out = append(out, row)
		}
	}
	return out
}

// callMCPTool posts one /mcp/call and decodes the tool payload.
func callMCPTool(t *testing.T, name string, arguments map[string]interface{}) (int, map[string]interface{}, map[string]interface{}, string) {
	t.Helper()
	body := map[string]interface{}{"name": name}
	if arguments != nil {
		body["arguments"] = arguments
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal call: %v", err)
	}
	status, data, code := callMCP(t, handleMCPCallReal, http.MethodPost, "/mcp/call", string(raw))
	decoded := mcpMap(t, data)
	result, _ := decoded["result"].(map[string]interface{})
	return status, decoded, result, code
}

func addMCPDevice(t *testing.T, cont *ServiceContainer, id, status string) {
	t.Helper()
	dev := &models.DeviceResponse{
		DeviceID:        id,
		Name:            id + " name",
		Protocol:        "simulator",
		Status:          status,
		Collecting:      status == "online",
		CollectInterval: 5,
		Points: []models.PointDef{
			{Name: "temperature", DataType: "float", Unit: "degC"},
			{Name: "pressure", DataType: "float", Unit: "bar"},
		},
	}
	if err := cont.DeviceRepo.Create(dev, "test"); err != nil {
		t.Fatalf("create device %s: %v", id, err)
	}
}

func TestMCPCallRunsToolsAgainstTheRealStore(t *testing.T) {
	cont := withMCPStore(t)
	addMCPDevice(t, cont, "mcp-online", "online")
	addMCPDevice(t, cont, "mcp-offline", "offline")

	status, data, result, code := callMCPTool(t, "list_devices", nil)
	if status != http.StatusOK {
		t.Fatalf("list_devices returned %d (%s)", status, code)
	}
	if data["name"] != "list_devices" {
		t.Errorf("echo of the tool name missing: %#v", data["name"])
	}
	devices := mcpRows(result["devices"])
	if len(devices) != 2 {
		t.Fatalf("list_devices saw %d rows, the store holds 2: %#v", len(devices), result)
	}
	first := devices[0]
	if first["device_id"] != "mcp-offline" && first["device_id"] != "mcp-online" {
		t.Errorf("device rows do not carry the ids that were created: %#v", first)
	}
	if first["point_count"] != float64(2) {
		t.Errorf("point_count = %#v, want the 2 points the device defines", first["point_count"])
	}
	// devices has no collecting column: the scheduler is the only source that
	// knows, so with no scheduler wired the tool has to say "unknown" rather
	// than report false for every device.
	if _, ok := first["collecting"]; ok && first["collecting"] != nil {
		t.Errorf("collecting reported %#v without a scheduler to ask", first["collecting"])
	}
	if result["total_in_store"] != float64(2) {
		t.Errorf("total_in_store = %#v, want 2", result["total_in_store"])
	}

	// The status filter has to come from the rows, not from a hardcoded 200.
	_, _, filtered, _ := callMCPTool(t, "list_devices", map[string]interface{}{"status": "offline"})
	rows := mcpRows(filtered["devices"])
	if len(rows) != 1 {
		t.Fatalf("status=offline matched %d rows, want 1: %#v", len(rows), filtered)
	}
	if rows[0]["device_id"] != "mcp-offline" || rows[0]["status"] != "offline" {
		t.Errorf("status=offline returned %#v, want mcp-offline/offline", rows[0])
	}
	if filtered["status_filter"] != "offline" {
		t.Errorf("the filter was not reported back: %#v", filtered["status_filter"])
	}
}

func TestMCPToolListMatchesTheCallableTools(t *testing.T) {
	cont := withMCPStore(t)
	addMCPDevice(t, cont, "mcp-listed", "online")
	tools := mcpListedTools(t)
	if len(tools) == 0 {
		t.Fatal("/mcp/tools is empty: RegisterMCPTools is not wired into startup")
	}
	for _, tool := range tools {
		name, _ := tool["name"].(string)
		if name == "" {
			t.Fatalf("tool without a name: %#v", tool)
		}
		if tool["description"] == nil || tool["description"] == "" {
			t.Errorf("tool %s has no description, so the table shows nothing", name)
		}
		// Every listed name has to be callable with the arguments it advertises:
		// the old page listed nothing and accepted everything, which is the exact
		// inverse of a working registry.
		status, _, _, code := callMCPTool(t, name, mcpArgsForSchema(t, tool, "mcp-listed"))
		if status == http.StatusNotFound && strings.Contains(code, "not a registered tool") {
			t.Fatalf("tool %s is listed but not callable", name)
		}
		if status == http.StatusInternalServerError {
			t.Fatalf("tool %s panicked or failed: %s", name, code)
		}
		if status == http.StatusBadRequest && strings.Contains(code, "ERR_MCP_UNKNOWN_ARGUMENT") {
			t.Fatalf("tool %s rejects the arguments its own schema declares required: %s", name, code)
		}
	}
}

// mcpListedTools snapshots /mcp/tools, which is the only place a client learns
// what a tool accepts.
func mcpListedTools(t *testing.T) []map[string]interface{} {
	t.Helper()
	_, listed, _ := callMCP(t, handleListMCPTools, http.MethodGet, "/mcp/tools", "")
	return mcpList(t, listed)
}

// mcpArgsForSchema fills the arguments a tool declares as required. A required key
// this helper cannot fill fails the test instead of skipping the call, because
// that means the schema asks for something no caller can supply.
func mcpArgsForSchema(t *testing.T, tool map[string]interface{}, deviceID string) map[string]interface{} {
	t.Helper()
	schema, _ := tool["parameters"].(map[string]interface{})
	if schema == nil {
		t.Fatalf("tool %v declares no parameters, so a client reading /mcp/tools cannot call it", tool["name"])
	}
	required, _ := schema["required"].([]interface{})
	args := map[string]interface{}{}
	for _, item := range required {
		key, _ := item.(string)
		switch key {
		case "device_id":
			args[key] = deviceID
		case "":
			t.Fatalf("tool %v has an empty entry in required: %#v", tool["name"], required)
		default:
			t.Fatalf("tool %v requires %q, which this test has no value for", tool["name"], key)
		}
	}
	return args
}

// TestMCPToolSchemasMatchWhatTheHandlersRead: declaring a schema is only worth
// something if the handler honours it in both directions - it must reject a key
// the schema never mentions, and every required key must be a declared property.
func TestMCPToolSchemasMatchWhatTheHandlersRead(t *testing.T) {
	cont := withMCPStore(t)
	addMCPDevice(t, cont, "mcp-schema", "online")

	for _, tool := range mcpListedTools(t) {
		name, _ := tool["name"].(string)
		schema, _ := tool["parameters"].(map[string]interface{})
		if schema == nil || schema["type"] != "object" {
			t.Errorf("tool %s does not declare an object schema: %#v", name, tool["parameters"])
			continue
		}
		props, _ := schema["properties"].(map[string]interface{})
		if props == nil {
			t.Errorf("tool %s has no properties map: %#v", name, schema)
			continue
		}
		required, _ := schema["required"].([]interface{})
		for _, item := range required {
			key, _ := item.(string)
			if _, ok := props[key]; !ok {
				t.Errorf("tool %s requires %q without declaring it as a property", name, key)
			}
		}
		// Each declared property needs a description, or the operator's parameter
		// box is still a guess.
		for key, raw := range props {
			prop, _ := raw.(map[string]interface{})
			if s, _ := prop["description"].(string); s == "" {
				t.Errorf("tool %s declares %s with no description", name, key)
			}
		}
	}

	// A mistyped key used to be ignored, which made a filtered call look like an
	// unfiltered one.
	if status, _, _, code := callMCPTool(t, "list_devices", map[string]interface{}{"limitz": 5}); status != http.StatusBadRequest ||
		!strings.Contains(code, "ERR_MCP_UNKNOWN_ARGUMENT") || !strings.Contains(code, "limit") {
		t.Errorf("an undeclared argument returned %d/%s, want 400 ERR_MCP_UNKNOWN_ARGUMENT naming limit", status, code)
	}
	// get_system_status declares no arguments at all, so sending one is a mistake.
	if status, _, _, code := callMCPTool(t, "get_system_status", map[string]interface{}{"device_id": "mcp-schema"}); status != http.StatusBadRequest ||
		!strings.Contains(code, "ERR_MCP_UNKNOWN_ARGUMENT") {
		t.Errorf("get_system_status accepted device_id with %d/%s, want 400", status, code)
	}
	// A declared key has to be accepted by the same call.
	if status, _, _, code := callMCPTool(t, "list_devices", map[string]interface{}{"limit": 1}); status != http.StatusOK {
		t.Errorf("a declared argument was refused: %d/%s", status, code)
	}
}

func TestMCPCallRefusesUnknownToolsAndBadParameters(t *testing.T) {
	cont := withMCPStore(t)
	addMCPDevice(t, cont, "mcp-online", "online")

	// A name that is not registered used to answer 200 with an empty object,
	// which a client reads as "the tool ran and found nothing".
	status, _, _, code := callMCPTool(t, "get_the_answer", nil)
	if status != http.StatusNotFound {
		t.Fatalf("unknown tool returned %d, want 404 (%s)", status, code)
	}
	if !strings.Contains(code, "list_devices") {
		t.Errorf("the refusal should name the tools that do exist: %s", code)
	}

	if status, _, _, code := callMCPTool(t, "   ", nil); status != http.StatusBadRequest || !strings.Contains(code, "ERR_MCP_MISSING_PARAMS") {
		t.Errorf("blank tool name returned %d/%s, want 400/ERR_MCP_MISSING_PARAMS", status, code)
	}

	// device_id is the tool's own contract, so the failure has to say which
	// parameter is missing rather than return an empty reading.
	if status, _, _, code := callMCPTool(t, "read_device_points", nil); status != http.StatusBadRequest || !strings.Contains(code, "device_id") {
		t.Errorf("missing device_id returned %d/%s, want 400 naming device_id", status, code)
	}
	if status, _, _, code := callMCPTool(t, "get_device_status", map[string]interface{}{"device_id": "nope"}); status != http.StatusNotFound || !strings.Contains(code, "nope") {
		t.Errorf("unknown device returned %d/%s, want 404 naming the device", status, code)
	}

	if status, _, code := callMCP(t, handleMCPCallReal, http.MethodPost, "/mcp/call", "not json"); status != http.StatusBadRequest {
		t.Errorf("unparseable body returned %d/%s, want 400", status, code)
	}
}

func TestMCPReadDevicePointsSeparatesNoDataFromZero(t *testing.T) {
	cont := withMCPStore(t)
	addMCPDevice(t, cont, "mcp-live", "online")
	addMCPDevice(t, cont, "mcp-silent", "online")

	if err := cont.TsStorage.WritePoints([]storage.PointData{
		{DeviceID: "mcp-live", PointName: "temperature", Value: 21.5, Quality: "GOOD", Timestamp: time.Now().Add(-3 * time.Second)},
		{DeviceID: "mcp-live", PointName: "pressure", Value: 4.25, Quality: "GOOD", Timestamp: time.Now().Add(-time.Second)},
	}); err != nil {
		t.Fatalf("write points: %v", err)
	}

	_, _, result, code := callMCPTool(t, "read_device_points", map[string]interface{}{"device_id": "mcp-live"})
	if code != "" {
		t.Fatalf("read_device_points failed: %s", code)
	}
	points, _ := result["points"].([]interface{})
	if len(points) != 2 {
		t.Fatalf("read %d points, want 2: %#v", len(points), result)
	}
	if result["count"] != float64(2) {
		t.Errorf("count = %#v, want 2", result["count"])
	}
	if _, ok := result["last_sample_at"].(string); !ok {
		t.Errorf("no last_sample_at in %#v", result)
	}
	var temperature map[string]interface{}
	for _, p := range points {
		row, _ := p.(map[string]interface{})
		if row["point_name"] == "temperature" {
			temperature = row
		}
	}
	if temperature == nil {
		t.Fatalf("temperature missing from %#v", points)
	}
	if temperature["value"] != 21.5 || temperature["quality"] != "GOOD" {
		t.Errorf("temperature read as %#v, want value 21.5 quality GOOD", temperature)
	}

	// A device that exists but has never reported must say so instead of
	// answering an empty list that looks like a reading of nothing.
	_, _, quiet, _ := callMCPTool(t, "read_device_points", map[string]interface{}{"device_id": "mcp-silent"})
	if n, _ := quiet["count"].(float64); n != 0 {
		t.Fatalf("a device with no samples reported %v points", quiet["count"])
	}
	note, _ := quiet["note"].(string)
	if !strings.Contains(note, "never collected") {
		t.Errorf("empty reading without an explanation: %#v", quiet)
	}

	// Asking for a point that has no data names it: silently dropping it would
	// read as "that point is fine".
	_, _, narrowed, _ := callMCPTool(t, "read_device_points", map[string]interface{}{
		"device_id":   "mcp-live",
		"point_names": []interface{}{"temperature", "vibration"},
	})
	rows, _ := narrowed["points"].([]interface{})
	if len(rows) != 1 {
		t.Fatalf("point filter kept %d rows: %#v", len(rows), narrowed)
	}
	missing, _ := narrowed["points_without_data"].([]interface{})
	if len(missing) != 1 || missing[0] != "vibration" {
		t.Errorf("points_without_data = %#v, want [vibration]", narrowed["points_without_data"])
	}
}

func TestMCPGetDeviceStatusReportsStoreTruth(t *testing.T) {
	cont := withMCPStore(t)
	addMCPDevice(t, cont, "mcp-live", "online")
	if err := cont.TsStorage.WritePoints([]storage.PointData{
		{DeviceID: "mcp-live", PointName: "temperature", Value: 1, Quality: "GOOD", Timestamp: time.Now()},
	}); err != nil {
		t.Fatalf("write points: %v", err)
	}
	_, _, result, code := callMCPTool(t, "get_device_status", map[string]interface{}{"device_id": "mcp-live"})
	if code != "" {
		t.Fatalf("get_device_status failed: %s", code)
	}
	if result["configured_points"] != float64(2) {
		t.Errorf("configured_points = %#v, want the 2 points the device defines", result["configured_points"])
	}
	if result["points_with_data"] != float64(1) {
		t.Errorf("points_with_data = %#v, want 1", result["points_with_data"])
	}
	if result["status"] != "online" {
		t.Errorf("status = %#v", result["status"])
	}
}

func TestMCPSystemStatusCountsWhatIsStored(t *testing.T) {
	cont := withMCPStore(t)
	addMCPDevice(t, cont, "mcp-a", "online")
	addMCPDevice(t, cont, "mcp-b", "offline")
	if err := cont.RuleRepo.Create(&models.RuleResponse{RuleID: "mcp-rule", Name: "hot", Logic: "and", Severity: "warning", DeviceID: "mcp-a", Enabled: true}, "test"); err != nil {
		t.Fatalf("create rule: %v", err)
	}
	if err := cont.AlarmRepo.Create(&models.AlarmResponse{
		AlarmID: "mcp-alarm", RuleID: "mcp-rule", DeviceID: "mcp-a",
		Severity: "warning", Status: "firing", Message: "temperature is high",
		FiredAt: time.Now().Format(time.RFC3339), RuleType: "threshold",
	}); err != nil {
		t.Fatalf("create alarm: %v", err)
	}

	_, _, status, code := callMCPTool(t, "get_system_status", nil)
	if code != "" {
		t.Fatalf("get_system_status failed: %s", code)
	}
	if status["device_total"] != float64(2) || status["device_online"] != float64(1) {
		t.Errorf("device counters = %v/%v, want 2/1", status["device_total"], status["device_online"])
	}
	if status["rule_total"] != float64(1) || status["rule_enabled"] != float64(1) {
		t.Errorf("rule counters = %v/%v, want 1/1", status["rule_total"], status["rule_enabled"])
	}
	if status["alarm_firing"] != float64(1) {
		t.Errorf("alarm_firing = %#v, want 1", status["alarm_firing"])
	}
	if status["version"] != gatewayVersion {
		t.Errorf("version = %#v, want the running %q", status["version"], gatewayVersion)
	}
	if _, ok := status["uptime_s"]; !ok {
		t.Errorf("no uptime_s in %#v", status)
	}

	// This container has no scheduler, so the tool cannot say whether collection
	// is running; "healthy" would be a claim it cannot support.
	if status["status"] != "degraded" {
		t.Errorf("status = %#v, want degraded while the scheduler is missing", status["status"])
	}
	if !strings.Contains(fmt.Sprint(status["missing_components"]), "scheduler") {
		t.Errorf("missing_components does not name the scheduler: %#v", status["missing_components"])
	}
	// Wiring the last missing component has to flip the verdict on its own, which
	// is what keeps status a derivation rather than a second literal.
	cont.Scheduler = engine.NewCollectScheduler(nil, nil, nil, &config.SchedulerConfig{})
	_, _, full, _ := callMCPTool(t, "get_system_status", nil)
	if full["status"] != "healthy" {
		t.Errorf("status = %#v with every component attached, want healthy", full["status"])
	}
	if _, ok := full["missing_components"]; ok {
		t.Errorf("missing_components is still reported on a full container: %#v", full["missing_components"])
	}
	if _, ok := full["scheduler"]; !ok {
		t.Errorf("the scheduler counters vanished: %#v", full)
	}

	_, _, alarms, _ := callMCPTool(t, "list_alarms", map[string]interface{}{"status": "firing"})
	rows, _ := alarms["alarms"].([]interface{})
	if len(rows) != 1 {
		t.Fatalf("list_alarms returned %d rows: %#v", len(rows), alarms)
	}
	alarm, _ := rows[0].(map[string]interface{})
	if alarm["alarm_id"] != "mcp-alarm" || alarm["message"] != "temperature is high" {
		t.Errorf("alarm row = %#v", alarm)
	}

	_, _, rules, _ := callMCPTool(t, "list_rules", nil)
	ruleRows, _ := rules["rules"].([]interface{})
	if len(ruleRows) != 1 {
		t.Fatalf("list_rules returned %d rows: %#v", len(ruleRows), rules)
	}
}

// TestMCPResourcesResolveThroughTheToolBus is the anti-drift check: a resource the
// page advertises has to name a tool that exists and actually answer, otherwise the
// table is decoration again.
func TestMCPResourcesResolveThroughTheToolBus(t *testing.T) {
	cont := withMCPStore(t)
	addMCPDevice(t, cont, "mcp-res", "online")

	_, rawResources, code := callMCP(t, handleMCPResourcesReal, http.MethodGet, "/mcp/resources", "")
	if code != "" {
		t.Fatalf("resources failed: %s", code)
	}
	payload := mcpMap(t, rawResources)
	resources := mcpRows(payload["resources"])
	if len(resources) == 0 {
		t.Fatalf("no resources reported: %#v", payload["resources"])
	}
	for _, res := range resources {
		tool, _ := res["tool"].(string)
		if tool == "" {
			t.Fatalf("resource %#v names no tool, so the row cannot be fetched", res)
		}
		args, _ := res["arguments"].(map[string]interface{})
		status, _, _, errCode := callMCPTool(t, tool, args)
		if status != http.StatusOK {
			t.Errorf("resource %v calls %s which returned %d (%s)", res["uri"], tool, status, errCode)
		}
	}
}

func TestMCPPromptsOnlyReferenceRegisteredTools(t *testing.T) {
	withMCPStore(t)
	_, rawPrompts, code := callMCP(t, handleMCPPromptsReal, http.MethodGet, "/mcp/prompts", "")
	if code != "" {
		t.Fatalf("prompts failed: %s", code)
	}
	payload := mcpMap(t, rawPrompts)
	prompts := mcpRows(payload["prompts"])
	if len(prompts) == 0 {
		t.Fatalf("no prompts reported: %#v", payload["prompts"])
	}
	registered := map[string]bool{}
	for _, tool := range mcpToolList(GetContainer()) {
		if name, ok := tool["name"].(string); ok {
			registered[name] = true
		}
	}
	for _, prompt := range prompts {
		steps, _ := prompt["steps"].([]interface{})
		if len(steps) == 0 {
			t.Fatalf("prompt %#v has no steps, so it is prose not a procedure", prompt["name"])
		}
		for i, s := range steps {
			step, _ := s.(map[string]interface{})
			tool, _ := step["tool"].(string)
			if !registered[tool] {
				t.Errorf("prompt %v step %d calls %q, which is not registered", prompt["name"], i, tool)
			}
		}
	}
}

func TestMCPAuthKeysAndSSEExplainWhatIsMissing(t *testing.T) {
	withMCPStore(t)

	status, rawKeys, code := callMCP(t, handleMCPAuthKeysReal, http.MethodGet, "/mcp/auth-keys", "")
	if status != http.StatusOK {
		t.Fatalf("GET /auth-keys returned %d (%s)", status, code)
	}
	payload := mcpMap(t, rawKeys)
	if keys := mcpRows(payload["keys"]); len(keys) != 0 {
		t.Errorf("this build stores no keys, but the list has %d: %#v", len(keys), payload["keys"])
	}
	if payload["enabled"] != true || payload["mechanism"] != "gateway_jwt" {
		t.Errorf("auth state should say a console JWT guards these routes: %#v", payload)
	}
	if note, _ := payload["note"].(string); !strings.Contains(note, "no MCP API-key store") {
		t.Errorf("empty list without an explanation: %#v", payload["note"])
	}

	// 201 for a credential nothing stores was the original lie; the page even
	// re-read the list to check, and warned the operator when it did not appear.
	for _, call := range []struct {
		method, path string
	}{
		{http.MethodPost, "/mcp/auth-keys"},
		{http.MethodDelete, "/mcp/auth-keys/k1"},
	} {
		status, _, code := callMCP(t, handleMCPAuthKeyUnsupported, call.method, call.path, `{"name":"bot","scopes":["read"]}`, "key_id", "k1")
		if status != http.StatusConflict || !strings.Contains(code, "ERR_MCP_UNSUPPORTED") {
			t.Errorf("%s %s returned %d/%s, want 409/ERR_MCP_UNSUPPORTED", call.method, call.path, status, code)
		}
	}

	// The old /mcp/sse wrote one comment and returned: a client would hang on a
	// stream that cannot carry an event.
	status, _, code = callMCP(t, handleMCPSSEUnsupported, http.MethodGet, "/mcp/sse", "")
	if status != http.StatusConflict || !strings.Contains(code, "ERR_MCP_UNSUPPORTED") {
		t.Errorf("GET /sse returned %d/%s, want 409/ERR_MCP_UNSUPPORTED", status, code)
	}
}

func TestMCPToolsReportAMissingDependencyInsteadOfPanicking(t *testing.T) {
	// A container without the optional repos wired is the common case in tests and
	// the degraded case in the field; the answer has to name the gap.
	prev := GetContainer()
	SetContainer(NewServiceContainer())
	cont := GetContainer()
	cont.MCPService = services.NewMCPService()
	RegisterMCPTools(cont)
	// The tools have to be reachable at all for the dependency refusals below to
	// be seen: the switch guards the routes ahead of them.
	setMCPServiceEnabled(true)
	t.Cleanup(func() { SetContainer(prev) })

	// Each tool names the dependency it is missing, with the code the page has
	// mapped since the Python service: a single generic code would make the
	// message "service unavailable" with no way to tell what to fix.
	for _, want := range []struct {
		tool string
		code string
	}{
		{"list_devices", "ERR_MCP_DEVICE_SERVICE_UNAVAILABLE"},
		{"list_alarms", "ERR_MCP_ALARM_SERVICE_UNAVAILABLE"},
		{"list_rules", "ERR_MCP_RULE_SERVICE_UNAVAILABLE"},
	} {
		status, _, _, code := callMCPTool(t, want.tool, nil)
		if status != http.StatusServiceUnavailable || !strings.Contains(code, want.code) {
			t.Errorf("%s with no repositories returned %d/%s, want 503/%s", want.tool, status, code, want.code)
		}
	}
	status, _, _, code := callMCPTool(t, "read_device_points", map[string]interface{}{"device_id": "x"})
	if status != http.StatusServiceUnavailable {
		t.Errorf("read_device_points with no repository returned %d/%s, want 503", status, code)
	}

	// get_system_status can still answer from what it has, but it must leave the
	// unknown counters out rather than report 0 devices for an unreachable store.
	sysStatus, payload, result, code := callMCPTool(t, "get_system_status", nil)
	if sysStatus != http.StatusOK {
		t.Fatalf("get_system_status refused outright with %d: %s", sysStatus, code)
	}
	if result == nil {
		t.Fatalf("no payload: %#v (%s)", payload, code)
	}
	for _, key := range []string{"device_total", "device_online", "rule_total", "alarm_firing"} {
		if _, ok := result[key]; ok {
			t.Errorf("%s reported as %#v with no repository wired", key, result[key])
		}
	}
	if result["version"] != gatewayVersion {
		t.Errorf("version = %#v, want %q", result["version"], gatewayVersion)
	}
}

func TestMCPParametersAreClamped(t *testing.T) {
	cont := withMCPStore(t)
	for i := 0; i < 5; i++ {
		addMCPDevice(t, cont, fmt.Sprintf("mcp-many-%d", i), "online")
	}
	// limit is a hint from an LLM, so it has to be bounded rather than obeyed.
	_, _, result, _ := callMCPTool(t, "list_devices", map[string]interface{}{"limit": 100000})
	if result["returned"] != float64(5) {
		t.Errorf("returned = %#v with all 5 rows expected under the clamp", result["returned"])
	}
	// The clamp has to be visible: silence here reads as "the store holds 5".
	if result["limit_requested"] != float64(100000) || result["limit_applied"] != float64(mcpMaxLimit) {
		t.Errorf("the clamp was not reported: %#v/%#v", result["limit_requested"], result["limit_applied"])
	}
	rows, _ := result["devices"].([]interface{})
	if len(rows) != 5 {
		t.Fatalf("devices = %d rows, want 5", len(rows))
	}
	_, _, paged, _ := callMCPTool(t, "list_devices", map[string]interface{}{"limit": 2})
	if paged["returned"] != float64(2) {
		t.Errorf("limit=2 returned %#v rows", paged["returned"])
	}
	if paged["total_in_store"] != float64(5) {
		t.Errorf("the clamp hid the real total: %#v", paged["total_in_store"])
	}
}

// mcpRoutesAreServed calls each MCP surface once and reports its HTTP status, so
// a test can assert "everything is served" or, after the switch is flipped,
// "everything refuses" with one loop.
func mcpRoutesAreServed(t *testing.T) map[string]int {
	t.Helper()
	out := map[string]int{}
	out["/mcp/call"], _, _ = callMCP(t, handleMCPCallReal, http.MethodPost, "/mcp/call",
		`{"name":"list_devices","arguments":{"limit":1}}`)
	out["/mcp/tools"], _, _ = callMCP(t, handleListMCPTools, http.MethodGet, "/mcp/tools", "")
	out["/mcp/resources"], _, _ = callMCP(t, handleMCPResourcesReal, http.MethodGet, "/mcp/resources", "")
	out["/mcp/prompts"], _, _ = callMCP(t, handleMCPPromptsReal, http.MethodGet, "/mcp/prompts", "")
	return out
}

// TestMCPServerSwitchGatesTheToolSurface pins the toggle the console's switch
// writes. Every one of these routes used to answer while the service reported
// itself switched off, so an operator who turned MCP off kept serving device,
// alarm and rule reads to any client with a console JWT and only ever saw the
// display change.
func TestMCPServerSwitchGatesTheToolSurface(t *testing.T) {
	withMCPStore(t) // the helper enables the service, so this starts on the served path

	for path, status := range mcpRoutesAreServed(t) {
		if status != http.StatusOK {
			t.Fatalf("%s answered %d while mcp_server is enabled", path, status)
		}
	}

	setMCPServiceEnabled(false)

	// The refusal has to name the way back on, otherwise the operator reading it
	// has a dead end rather than a procedure.
	for path, status := range mcpRoutesAreServed(t) {
		if status != http.StatusConflict {
			t.Errorf("%s answered %d with the switch off, want 409", path, status)
		}
	}
	_, refusedData, code := callMCP(t, handleMCPCallReal, http.MethodPost, "/mcp/call", `{"name":"list_devices"}`)
	if !strings.Contains(code, "ERR_MCP_DISABLED") {
		t.Errorf("refusal code = %s, want ERR_MCP_DISABLED", code)
	}
	if !strings.Contains(code, "/services/mcp_server/enable") {
		t.Errorf("the refusal should say how to turn the service back on: %s", code)
	}
	if len(refusedData) > 0 && string(refusedData) != "null" {
		t.Errorf("a refused call still carried a payload: %s", refusedData)
	}

	// The status routes must agree with the behaviour, not with the registry.
	_, statusData, _ := callMCP(t, handleGetMCPStatus, http.MethodGet, "/mcp/status", "")
	status := mcpMap(t, statusData)
	if status["running"] != false || status["refuses_calls"] != true {
		t.Errorf("/mcp/status = %#v while every call is refused", status)
	}
	if status["tool_count"] != float64(6) {
		t.Errorf("tool_count = %#v, want the registered tools still counted so the page can say so", status["tool_count"])
	}
	_, svcData, _ := callMCP(t, handleGetServiceStatus, http.MethodGet, "/services/mcp_server/status", "", "name", "mcp_server")
	svc := mcpMap(t, svcData)
	if svc["state"] != "disabled" || svc["enabled"] != false {
		t.Errorf("/services/mcp_server/status = %#v while the routes refuse", svc)
	}

	// GET /mcp/config is the other view of the same switch; it cannot report the
	// file's value while the toggle decides what the gateway does.
	_, cfgData, _ := callMCP(t, handleGetMCPConfig, http.MethodGet, "/mcp/config", "")
	if cfg := mcpMap(t, cfgData); cfg["enabled"] != false {
		t.Errorf("/mcp/config reports enabled=%#v while calls are refused", cfg["enabled"])
	}

	setMCPServiceEnabled(true)
	for path, status := range mcpRoutesAreServed(t) {
		if status != http.StatusOK {
			t.Errorf("%s answered %d after the service was switched back on, want 200", path, status)
		}
	}
}

// TestMCPConfigFileSwitchIsTheDefaultForTheRoutes covers the other half of the
// predicate: with no console toggle recorded, mcp_server.enabled in the config
// file decides - a field that no code read until now, which is why a gateway
// configured with MCP off answered every tool call.
func TestMCPConfigFileSwitchIsTheDefaultForTheRoutes(t *testing.T) {
	withIsolatedConfig(t)
	cont := GetContainer()
	cont.MCPService = services.NewMCPService()
	RegisterMCPTools(cont)
	cfg := config.GetConfig()

	cfg.McpServer.Enabled = false
	if _, _, code := callMCP(t, handleListMCPTools, http.MethodGet, "/mcp/tools", ""); !strings.Contains(code, "ERR_MCP_DISABLED") {
		t.Fatalf("/mcp/tools answered despite mcp_server.enabled being false: %s", code)
	}

	cfg.McpServer.Enabled = true
	if status, toolsData, _ := callMCP(t, handleListMCPTools, http.MethodGet, "/mcp/tools", ""); status != http.StatusOK {
		t.Fatalf("/mcp/tools with mcp_server.enabled: true returned %d", status)
	} else if len(mcpList(t, toolsData)) != 6 {
		t.Fatalf("the config-on path listed %s, want the six registered tools", toolsData)
	}

	// An explicit operator toggle wins over the file, in both directions: this is
	// what lets somebody switch the service off on a gateway that ships it on.
	setMCPServiceEnabled(false)
	if status, _, _ := callMCP(t, handleListMCPTools, http.MethodGet, "/mcp/tools", ""); status != http.StatusConflict {
		t.Errorf("/mcp/tools returned %d with the operator switch off and the file on, want 409", status)
	}
	if status, data, _ := callMCP(t, handleGetServiceStatus, http.MethodGet, "/services/mcp_server/status", "", "name", "mcp_server"); status != http.StatusOK {
		t.Fatalf("/services/mcp_server/status returned %d: %s", status, data)
	} else if svc := mcpMap(t, data); svc["state"] != "disabled" {
		t.Errorf("the service list state = %#v, want disabled to match what the routes do", svc["state"])
	}
}
