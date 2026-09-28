package api

// /api/v1/mcp/* was a set of echoes: /call answered {"result":{}} for any body,
// /resources, /prompts and /auth-keys answered an empty list that the page then
// rendered as "the MCP server has no data", POST /auth-keys returned 201 with the
// request echoed back, and GET /sse wrote one comment line and closed - an
// "MCP server" that had no tools, no credential store and no transport.
//
// services.MCPService was a real tool registry with one problem: nothing ever
// called RegisterTool outside the tests, so the whole surface was wired to an
// empty map. RegisterMCPTools now populates it from the container's own
// repositories, so /mcp/tools lists what actually exists and /mcp/call runs it.
//
// What is deliberately NOT implemented, and now says so instead of returning 200:
//   - API keys. Every /mcp route sits behind the same AuthMiddleware + permission
//     check as the console, so a generated key would authenticate nothing.
//   - The SSE transport. Speaking MCP over SSE means a JSON-RPC loop and a
//     session, neither of which exists here; a half stream that closes is worse
//     than a refusal, because a client would hang waiting for the first event.
//   - /mcp/sse-ticket, which had no caller and minted a ticket nothing checked;
//     it is not mounted at all.

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"edgelite/internal/models"
	"edgelite/internal/security"
	"edgelite/internal/services"
)

// mcpToolError lets a tool report why it failed in a way the HTTP layer can map:
// tools return (interface{}, error) and have no notion of status codes.
type mcpToolError struct {
	Code   string
	Detail string
}

func (e mcpToolError) Error() string { return e.Code + ": " + e.Detail }

func newMCPToolError(code, format string, args ...interface{}) error {
	return mcpToolError{Code: code, Detail: fmt.Sprintf(format, args...)}
}

// mcpToolMaxLimits bound what one tool call can pull out of the store: an MCP
// client is an LLM, and a runaway "list everything" should not page through a
// million alarms.
const (
	mcpDefaultLimit = 100
	mcpMaxLimit     = 1000
)

// RegisterMCPRealRoutes mounts /mcp/call, /mcp/resources, /mcp/prompts and
// /mcp/auth-keys, replacing RegisterMCPExtendedRoutes. /mcp/status, /mcp/tools
// and /mcp/config come from RegisterMCPRoutes and are unchanged.
func RegisterMCPRealRoutes(g *echo.Group) {
	g.POST("/call", handleMCPCallReal, requirePermission(security.PermSystemConfig))
	g.GET("/resources", handleMCPResourcesReal, requirePermission(security.PermSystemConfig))
	g.GET("/prompts", handleMCPPromptsReal, requirePermission(security.PermSystemConfig))
	g.GET("/auth-keys", handleMCPAuthKeysReal, requirePermission(security.PermSystemConfig))
	g.POST("/auth-keys", handleMCPAuthKeyUnsupported, requirePermission(security.PermSystemConfig))
	g.DELETE("/auth-keys/:key_id", handleMCPAuthKeyUnsupported, requirePermission(security.PermSystemConfig))
	g.GET("/sse", handleMCPSSEUnsupported, requirePermission(security.PermSystemConfig))
}

// RegisterMCPTools populates the container's tool registry. It is called from
// main once the services exist, and is safe to call on a partially wired
// container: each tool checks its own dependency at call time so a missing
// repository is reported as a capability gap rather than a panic.
func RegisterMCPTools(cont *ServiceContainer) {
	if cont == nil || cont.MCPService == nil {
		return
	}
	svc := cont.MCPService
	for _, tool := range []services.MCPTool{
		{
			Name:        "list_devices",
			Description: "List the devices this gateway manages, with protocol and online status.",
			Parameters: mcpSchema(nil, mcpProp(
				mcpStr("status", "Return only devices stored with this status, e.g. online or offline."),
				mcpInt("limit", "Maximum rows to return.", mcpDefaultLimit, mcpMaxLimit),
			)),
			Handler: func(params map[string]interface{}) (interface{}, error) { return mcpListDevices(cont, params) },
		},
		{
			Name:        "get_device_status",
			Description: "Read one device's configuration status and the timestamp of its last sample.",
			Parameters: mcpSchema([]string{"device_id"}, mcpProp(
				mcpStr("device_id", "The device to inspect."),
			)),
			Handler: func(params map[string]interface{}) (interface{}, error) { return mcpGetDeviceStatus(cont, params) },
		},
		{
			Name:        "read_device_points",
			Description: "Read the latest value, quality and timestamp of a device's points.",
			Parameters: mcpSchema([]string{"device_id"}, mcpProp(
				mcpStr("device_id", "The device whose samples to read."),
				mcpPointNames("point_names"),
			)),
			Handler: func(params map[string]interface{}) (interface{}, error) { return mcpReadDevicePoints(cont, params) },
		},
		{
			Name:        "list_alarms",
			Description: "List alarms, optionally filtered by status, severity or device.",
			Parameters: mcpSchema(nil, mcpProp(
				mcpStr("status", "Return only alarms stored with this status, e.g. firing, acknowledged or recovered."),
				mcpStr("severity", "Return only alarms of this severity, e.g. critical, major or minor."),
				mcpStr("device_id", "Return only alarms raised for this device."),
				mcpInt("limit", "Maximum rows to return.", mcpDefaultLimit, mcpMaxLimit),
			)),
			Handler: func(params map[string]interface{}) (interface{}, error) { return mcpListAlarms(cont, params) },
		},
		{
			Name:        "list_rules",
			Description: "List the alarm rules that are configured, with their enabled state.",
			Parameters: mcpSchema(nil, mcpProp(
				mcpStr("device_id", "Return only rules bound to this device."),
				mcpInt("limit", "Maximum rows to return.", mcpDefaultLimit, mcpMaxLimit),
			)),
			Handler: func(params map[string]interface{}) (interface{}, error) { return mcpListRules(cont, params) },
		},
		{
			Name:        "get_system_status",
			Description: "Read gateway uptime, resource usage and the device/rule/alarm counters.",
			Parameters:  mcpSchema(nil, map[string]interface{}{}),
			Handler:     func(params map[string]interface{}) (interface{}, error) { return mcpGetSystemStatus(cont, params) },
		},
	} {
		svc.RegisterTool(tool)
	}
}

// --- tool argument schemas ---
//
// A tool call crosses the process boundary as an untyped map, so the registry
// has to state which keys it reads. /mcp/call rejects a key that is not listed
// here rather than ignoring it: a caller that sends device_name instead of
// device_id would otherwise get an unfiltered list and read it as a real answer.

func mcpSchema(required []string, props map[string]interface{}) map[string]interface{} {
	if required == nil {
		required = []string{}
	}
	return map[string]interface{}{
		"type":       "object",
		"properties": props,
		"required":   required,
	}
}

func mcpProp(entries ...map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{}
	for _, entry := range entries {
		for k, v := range entry {
			out[k] = v
		}
	}
	return out
}

func mcpStr(name, description string) map[string]interface{} {
	return map[string]interface{}{name: map[string]interface{}{"type": "string", "description": description}}
}

func mcpInt(name, description string, def, maximum int) map[string]interface{} {
	return map[string]interface{}{name: map[string]interface{}{
		"type":        "integer",
		"description": description,
		"default":     def,
		"minimum":     1,
		"maximum":     maximum,
	}}
}

// mcpPointNames documents the one argument that accepts two shapes.
func mcpPointNames(name string) map[string]interface{} {
	return map[string]interface{}{name: map[string]interface{}{
		"description": "Only return samples for these points.",
		"oneOf": []interface{}{
			map[string]interface{}{"type": "string", "description": "comma-separated, e.g. \"temp,pressure\""},
			map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
		},
	}}
}

// mcpUnknownArgs lists the arguments a tool never reads, keyed against its
// declared schema. A tool without a schema is not validated here because it
// promises no argument contract.
func mcpUnknownArgs(schema map[string]interface{}, params map[string]interface{}) []string {
	if schema == nil || params == nil {
		return nil
	}
	props, _ := schema["properties"].(map[string]interface{})
	unknown := make([]string, 0, len(params))
	for key := range params {
		if _, ok := props[key]; !ok {
			unknown = append(unknown, key)
		}
	}
	sort.Strings(unknown)
	return unknown
}

// refuseWhenMCPOff writes the refusal for the tool surface while the service is
// switched off and reports whether it did. Until now the operator's switch only
// changed what /mcp/status and the console page displayed: with mcp_server
// disabled every tool still answered, so a gateway that looked closed to its
// operator was serving device, alarm and rule reads to any client holding a
// console JWT.
//
// It answers with a bool rather than an error because this package's response
// helpers return whatever c.JSON returned - nil on a successful write - so an
// `if err := guard(c); err != nil` would fall straight through and write a
// second body.
func refuseWhenMCPOff(c echo.Context) bool {
	if mcpServiceEnabled() {
		return false
	}
	Conflict(c, "ERR_MCP_DISABLED: mcp_server is switched off, so this build serves no MCP tools - "+
		"enable it with POST /api/v1/services/mcp_server/enable or mcp_server.enabled in the config file, then retry")
	return true
}

// handleMCPCallReal runs a registered tool. An unknown name is a 404 that lists
// what does exist: the previous handler answered 200 with an empty result for any
// name, which let a client conclude the tool worked and simply returned nothing.
//
// The code names below are not invented here: frontend/src/utils/errorCodes.ts
// already mapped ERR_MCP_UNKNOWN_TOOL, ERR_MCP_MISSING_DEVICE_ID,
// ERR_MCP_DEVICE_NOT_FOUND and the per-service *_SERVICE_UNAVAILABLE codes, but no
// Go handler ever emitted them, because the /mcp routes were echoes that could not
// fail. Reusing that vocabulary gives the page a translation for every refusal and
// retires the dead mappings instead of adding a parallel set.
func handleMCPCallReal(c echo.Context) error {
	if refuseWhenMCPOff(c) {
		return nil
	}
	var req struct {
		Name      string                 `json:"name"`
		Arguments map[string]interface{} `json:"arguments"`
	}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, `ERR_MCP_MISSING_PARAMS: /mcp/call expects {"name":"...","arguments":{...}}`)
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return BadRequest(c, "ERR_MCP_MISSING_PARAMS: no tool name was sent")
	}
	cont := GetContainer()
	if cont == nil || cont.MCPService == nil {
		return ServiceUnavailable(c, "ERR_MCP_SYSTEM_SERVICE_UNAVAILABLE: no tool registry is attached to this build")
	}
	registered := mcpToolList(cont)
	var matched map[string]interface{}
	names := make([]string, 0, len(registered))
	for _, tool := range registered {
		name, _ := tool["name"].(string)
		names = append(names, name)
		if name == req.Name {
			matched = tool
		}
	}
	if matched == nil {
		return NotFound(c, fmt.Sprintf("ERR_MCP_UNKNOWN_TOOL: %s is not a registered tool; this build registers: %s", name, strings.Join(names, ", ")))
	}
	if schema, _ := matched["parameters"].(map[string]interface{}); schema != nil {
		if unknown := mcpUnknownArgs(schema, req.Arguments); len(unknown) > 0 {
			accepted := make([]string, 0)
			if props, ok := schema["properties"].(map[string]interface{}); ok {
				for key := range props {
					accepted = append(accepted, key)
				}
			}
			sort.Strings(accepted)
			return BadRequest(c, fmt.Sprintf("ERR_MCP_UNKNOWN_ARGUMENT: %s does not accept %s; it accepts: %s",
				name, strings.Join(unknown, ", "), strings.Join(accepted, ", ")))
		}
	}
	result, err := cont.MCPService.CallTool(name, req.Arguments)
	if err != nil {
		return mcpToolErrorResponse(c, err)
	}
	return OK(c, map[string]interface{}{"name": name, "result": result})
}

func mcpToolErrorResponse(c echo.Context, err error) error {
	var toolErr mcpToolError
	if !errors.As(err, &toolErr) {
		return InternalError(c, "ERR_MCP_CALL_FAILED: "+err.Error())
	}
	msg := toolErr.Code + ": " + toolErr.Detail
	switch {
	case toolErr.Code == "ERR_MCP_UNKNOWN_TOOL", toolErr.Code == "ERR_MCP_DEVICE_NOT_FOUND":
		return NotFound(c, msg)
	case toolErr.Code == "ERR_MCP_MISSING_PARAMS", toolErr.Code == "ERR_MCP_MISSING_DEVICE_ID":
		return BadRequest(c, msg)
	case strings.HasSuffix(toolErr.Code, "_SERVICE_UNAVAILABLE"):
		return ServiceUnavailable(c, msg)
	default:
		return InternalError(c, msg)
	}
}

// handleMCPResourcesReal answers the resources the gateway can actually resolve:
// each one names a registered tool plus the arguments that materialise it, so a
// client can fetch the row it was shown through /mcp/call. mcp_real_test.go
// asserts every listed tool exists, which is what keeps this list honest as the
// registry changes.
func handleMCPResourcesReal(c echo.Context) error {
	if refuseWhenMCPOff(c) {
		return nil
	}
	resources := mcpStaticResources()
	return OK(c, map[string]interface{}{
		"resources": resources,
		"total":     len(resources),
	})
}

func handleMCPPromptsReal(c echo.Context) error {
	if refuseWhenMCPOff(c) {
		return nil
	}
	prompts := mcpStaticPrompts()
	return OK(c, map[string]interface{}{
		"prompts": prompts,
		"total":   len(prompts),
	})
}

func mcpStaticResources() []map[string]interface{} {
	return []map[string]interface{}{
		{
			"uri":         "edgelite://devices",
			"name":        "devices",
			"description": "Every device with its protocol, status and point count.",
			"mime_type":   "application/json",
			"tool":        "list_devices",
			"arguments":   map[string]interface{}{},
		},
		{
			"uri":         "edgelite://alarms/active",
			"name":        "alarms/active",
			"description": "Alarms currently firing.",
			"mime_type":   "application/json",
			"tool":        "list_alarms",
			"arguments":   map[string]interface{}{"status": "firing"},
		},
		{
			"uri":         "edgelite://system/status",
			"name":        "system/status",
			"description": "Gateway uptime, resources and counters.",
			"mime_type":   "application/json",
			"tool":        "get_system_status",
			"arguments":   map[string]interface{}{},
		},
	}
}

// mcpStaticPrompts are procedures over the registered tools rather than canned
// text: the gateway has no model to render a template, so what it can offer is
// the ordered set of tool calls that answer the question.
func mcpStaticPrompts() []map[string]interface{} {
	return []map[string]interface{}{
		{
			"name":        "analyze_device",
			"description": "Work out whether one device is healthy: status, then its latest values, then its alarms.",
			"arguments": []map[string]interface{}{
				{"name": "device_id", "required": true, "description": "The device to inspect."},
			},
			"steps": []map[string]interface{}{
				{"tool": "get_device_status", "arguments": map[string]interface{}{"device_id": "{device_id}"}},
				{"tool": "read_device_points", "arguments": map[string]interface{}{"device_id": "{device_id}"}},
				{"tool": "list_alarms", "arguments": map[string]interface{}{"device_id": "{device_id}"}},
			},
		},
		{
			"name":        "alarm_summary",
			"description": "Summarise what is firing right now next to the gateway's own health.",
			"arguments":   []map[string]interface{}{},
			"steps": []map[string]interface{}{
				{"tool": "list_alarms", "arguments": map[string]interface{}{"status": "firing"}},
				{"tool": "get_system_status", "arguments": map[string]interface{}{}},
			},
		},
	}
}

// handleMCPAuthKeysReal reports the credential situation as it is: the console's
// JWT guards these routes, and there is no key store to list.
func handleMCPAuthKeysReal(c echo.Context) error {
	return OK(c, map[string]interface{}{
		"keys":      []interface{}{},
		"total":     0,
		"enabled":   true,
		"mechanism": "gateway_jwt",
		"note":      "these routes are authenticated by the same gateway JWT as the console; this build has no MCP API-key store, so creating a key is refused rather than accepted and forgotten",
	})
}

func handleMCPAuthKeyUnsupported(c echo.Context) error {
	return Conflict(c, "ERR_MCP_UNSUPPORTED: this build issues no MCP API keys - /mcp/* is reachable only with a console JWT that already carries the operator's permissions, so a generated key would authenticate nothing and could not be revoked")
}

func handleMCPSSEUnsupported(c echo.Context) error {
	return Conflict(c, "ERR_MCP_UNSUPPORTED: /mcp/sse is not served by this build - there is no JSON-RPC loop or MCP session behind it, so the stream would open and stay silent; the same tools are available synchronously through POST /mcp/call")
}

// --- tool parameter helpers ---

func mcpString(params map[string]interface{}, key string) string {
	if params == nil {
		return ""
	}
	switch v := params[key].(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return fmt.Sprintf("%t", v)
	default:
		return ""
	}
}

func mcpStringList(params map[string]interface{}, key string) []string {
	if params == nil {
		return nil
	}
	switch v := params[key].(type) {
	case string:
		out := []string{}
		for _, part := range strings.Split(v, ",") {
			if s := strings.TrimSpace(part); s != "" {
				out = append(out, s)
			}
		}
		return out
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	default:
		return nil
	}
}

// mcpLimit resolves the caller's row hint. It answers the requested value too so
// the tools can report a clamp: an LLM that asks for 100000 rows and gets 1000
// back has to be able to tell "the gateway caps reads" from "the store only
// holds this much".
func mcpLimit(params map[string]interface{}) (applied, requested int) {
	switch v := params["limit"].(type) {
	case float64:
		requested = int(v)
	case int:
		requested = v
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			requested = n
		}
	}
	applied = requested
	if applied <= 0 {
		applied = mcpDefaultLimit
	}
	if applied > mcpMaxLimit {
		applied = mcpMaxLimit
	}
	return applied, requested
}

// mcpNoteLimit records the clamp in the result when one happened.
func mcpNoteLimit(result map[string]interface{}, applied, requested int) {
	if requested > 0 && requested != applied {
		result["limit_requested"] = requested
		result["limit_applied"] = applied
		result["limit_note"] = fmt.Sprintf("the gateway caps one read at %d rows", mcpMaxLimit)
	}
}

func mcpRequiredDeviceID(params map[string]interface{}) (string, error) {
	id := mcpString(params, "device_id")
	if id == "" {
		return "", newMCPToolError("ERR_MCP_MISSING_DEVICE_ID", "device_id is required")
	}
	return id, nil
}

func mcpDeviceLookup(cont *ServiceContainer, deviceID string) (*models.DeviceResponse, error) {
	if cont == nil || cont.DeviceRepo == nil {
		return nil, newMCPToolError("ERR_MCP_DEVICE_SERVICE_UNAVAILABLE", "no device repository is attached to this build")
	}
	dev, err := cont.DeviceRepo.Get(deviceID)
	if err != nil {
		return nil, newMCPToolError("ERR_MCP_CALL_FAILED", "reading device %s failed: %v", deviceID, err)
	}
	// This repository reports a missing row as (nil, nil), so the nil check is the
	// 404 and a non-nil error is a storage failure.
	if dev == nil {
		return nil, newMCPToolError("ERR_MCP_DEVICE_NOT_FOUND", "no device with id %s is registered on this gateway", deviceID)
	}
	return dev, nil
}

// mcpCollecting answers whether a collector is running for a device. The stored
// row has no collecting column and the read path never fills it, so reporting
// DeviceResponse.Collecting would claim "not collecting" for every device; the
// scheduler is the only source that knows, and without one the answer is unknown.
func mcpCollecting(cont *ServiceContainer, deviceID string) interface{} {
	if cont == nil || cont.Scheduler == nil {
		return nil
	}
	return cont.Scheduler.IsCollecting(deviceID)
}

// mcpLatestPoints reads the time-series store, or a tool error when the gateway
// has no store wired at all.
func mcpLatestPoints(cont *ServiceContainer, deviceID string) (map[string]interface{}, error) {
	if cont == nil || cont.TsStorage == nil {
		return nil, newMCPToolError("ERR_MCP_SYSTEM_SERVICE_UNAVAILABLE", "no time-series store is attached to this build")
	}
	values, err := cont.TsStorage.GetLatestPoints(deviceID)
	if err != nil {
		return nil, newMCPToolError("ERR_MCP_CALL_FAILED", "reading the latest values failed: %v", err)
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	points := make([]map[string]interface{}, 0, len(names))
	var lastSeen time.Time
	for _, name := range names {
		p := values[name]
		ts := p.Timestamp.UTC().Format(time.RFC3339Nano)
		if p.Timestamp.After(lastSeen) {
			lastSeen = p.Timestamp
		}
		points = append(points, map[string]interface{}{
			"point_name": p.PointName,
			"value":      p.Value,
			"quality":    p.Quality,
			"timestamp":  ts,
		})
	}
	out := map[string]interface{}{
		"points": points,
		"count":  len(points),
	}
	if !lastSeen.IsZero() {
		out["last_sample_at"] = lastSeen.UTC().Format(time.RFC3339Nano)
	}
	return out, nil
}

// --- the tools themselves ---

func mcpListDevices(cont *ServiceContainer, params map[string]interface{}) (interface{}, error) {
	if cont == nil || cont.DeviceRepo == nil {
		return nil, newMCPToolError("ERR_MCP_DEVICE_SERVICE_UNAVAILABLE", "no device repository is attached to this build")
	}
	limit, requested := mcpLimit(params)
	statusFilter := mcpString(params, "status")
	devices, total, err := cont.DeviceRepo.List(1, mcpMaxLimit)
	if err != nil {
		return nil, newMCPToolError("ERR_MCP_CALL_FAILED", "listing devices failed: %v", err)
	}
	out := make([]map[string]interface{}, 0, len(devices))
	for _, d := range devices {
		if statusFilter != "" && d.Status != statusFilter {
			continue
		}
		if len(out) >= limit {
			break
		}
		out = append(out, map[string]interface{}{
			"device_id":        d.DeviceID,
			"name":             d.Name,
			"protocol":         d.Protocol,
			"status":           d.Status,
			"collecting":       mcpCollecting(cont, d.DeviceID),
			"point_count":      len(d.Points),
			"collect_interval": d.CollectInterval,
		})
	}
	result := map[string]interface{}{
		"devices":        out,
		"returned":       len(out),
		"total_in_store": total,
	}
	if statusFilter != "" {
		result["status_filter"] = statusFilter
	}
	if len(out) == 0 {
		result["note"] = mcpEmptyNote("no device matches", statusFilter)
	}
	mcpNoteLimit(result, limit, requested)
	return result, nil
}

func mcpGetDeviceStatus(cont *ServiceContainer, params map[string]interface{}) (interface{}, error) {
	deviceID, err := mcpRequiredDeviceID(params)
	if err != nil {
		return nil, err
	}
	dev, err := mcpDeviceLookup(cont, deviceID)
	if err != nil {
		return nil, err
	}
	result := map[string]interface{}{
		"device_id":         dev.DeviceID,
		"name":              dev.Name,
		"protocol":          dev.Protocol,
		"status":            dev.Status,
		"collecting":        mcpCollecting(cont, dev.DeviceID),
		"configured_points": len(dev.Points),
		"collect_interval":  dev.CollectInterval,
		"updated_at":        dev.UpdatedAt,
	}
	if points, perr := mcpLatestPoints(cont, deviceID); perr == nil {
		result["points_with_data"] = points["count"]
		if last, ok := points["last_sample_at"]; ok {
			result["last_sample_at"] = last
		}
	} else {
		// The device exists; only its samples are unreachable. Reporting that
		// separately keeps a storage gap from looking like a dead device.
		result["points_with_data"] = 0
		result["sample_note"] = perr.Error()
	}
	return result, nil
}

func mcpReadDevicePoints(cont *ServiceContainer, params map[string]interface{}) (interface{}, error) {
	deviceID, err := mcpRequiredDeviceID(params)
	if err != nil {
		return nil, err
	}
	if _, err := mcpDeviceLookup(cont, deviceID); err != nil {
		return nil, err
	}
	latest, err := mcpLatestPoints(cont, deviceID)
	if err != nil {
		return nil, err
	}
	// A point list narrows the answer without changing what the store holds, so
	// the filter is applied here rather than in the storage layer.
	if wanted := mcpStringList(params, "point_names"); len(wanted) > 0 {
		allow := make(map[string]bool, len(wanted))
		for _, name := range wanted {
			allow[name] = true
		}
		all, _ := latest["points"].([]map[string]interface{})
		filtered := make([]map[string]interface{}, 0, len(all))
		matched := make(map[string]bool, len(all))
		for _, p := range all {
			name, _ := p["point_name"].(string)
			matched[name] = true
			if allow[name] {
				filtered = append(filtered, p)
			}
		}
		missing := make([]string, 0, len(wanted))
		for _, name := range wanted {
			if !matched[name] {
				missing = append(missing, name)
			}
		}
		latest["points"] = filtered
		latest["count"] = len(filtered)
		latest["requested"] = len(wanted)
		if len(missing) > 0 {
			latest["points_without_data"] = missing
		}
	}
	result := latest
	result["device_id"] = deviceID
	if count, _ := result["count"].(int); count == 0 {
		result["note"] = "the gateway holds no sample for this device yet: an empty list means never collected, not a value of zero"
	}
	return result, nil
}

func mcpListAlarms(cont *ServiceContainer, params map[string]interface{}) (interface{}, error) {
	if cont == nil || cont.AlarmRepo == nil {
		return nil, newMCPToolError("ERR_MCP_ALARM_SERVICE_UNAVAILABLE", "no alarm repository is attached to this build")
	}
	limit, requested := mcpLimit(params)
	filter := models.AlarmFilter{
		Status:   mcpString(params, "status"),
		Severity: mcpString(params, "severity"),
		DeviceID: mcpString(params, "device_id"),
	}
	alarms, total, err := cont.AlarmRepo.List(filter, 1, limit)
	if err != nil {
		return nil, newMCPToolError("ERR_MCP_CALL_FAILED", "listing alarms failed: %v", err)
	}
	out := make([]map[string]interface{}, 0, len(alarms))
	for _, a := range alarms {
		out = append(out, map[string]interface{}{
			"alarm_id":      a.AlarmID,
			"rule_id":       a.RuleID,
			"device_id":     a.DeviceID,
			"severity":      a.Severity,
			"status":        a.Status,
			"message":       a.Message,
			"trigger_value": a.TriggerValue,
			"fired_at":      a.FiredAt,
			"recovered_at":  a.RecoveredAt,
		})
	}
	result := map[string]interface{}{
		"alarms":         out,
		"returned":       len(out),
		"total_matching": total,
	}
	if len(out) == 0 {
		result["note"] = mcpEmptyNote("no alarm matches", filter.Status+" "+filter.Severity+" "+filter.DeviceID)
	}
	mcpNoteLimit(result, limit, requested)
	return result, nil
}

func mcpListRules(cont *ServiceContainer, params map[string]interface{}) (interface{}, error) {
	if cont == nil || cont.RuleRepo == nil {
		return nil, newMCPToolError("ERR_MCP_RULE_SERVICE_UNAVAILABLE", "no rule repository is attached to this build")
	}
	limit, requested := mcpLimit(params)
	deviceID := mcpString(params, "device_id")
	rules, total, err := cont.RuleRepo.List(1, limit, deviceID)
	if err != nil {
		return nil, newMCPToolError("ERR_MCP_CALL_FAILED", "listing rules failed: %v", err)
	}
	out := make([]map[string]interface{}, 0, len(rules))
	for _, r := range rules {
		out = append(out, map[string]interface{}{
			"rule_id":    r.RuleID,
			"name":       r.Name,
			"device_id":  r.DeviceID,
			"severity":   r.Severity,
			"enabled":    r.Enabled,
			"rule_type":  r.RuleType,
			"logic":      r.Logic,
			"conditions": r.Conditions,
			"duration":   r.Duration,
		})
	}
	result := map[string]interface{}{
		"rules":          out,
		"returned":       len(out),
		"total_matching": total,
	}
	if len(out) == 0 {
		result["note"] = mcpEmptyNote("no rule is configured", deviceID)
	}
	mcpNoteLimit(result, limit, requested)
	return result, nil
}

func mcpGetSystemStatus(cont *ServiceContainer, params map[string]interface{}) (interface{}, error) {
	if cont == nil {
		return nil, newMCPToolError("ERR_MCP_SYSTEM_SERVICE_UNAVAILABLE", "no service container is attached to this build")
	}
	// "healthy" has to mean something: the counters below only cover the
	// components that are actually wired, so a build missing its alarm store
	// would otherwise report a clean bill of health with alarm_firing absent.
	missing := make([]string, 0, 6)
	for _, dep := range []struct {
		name    string
		present bool
	}{
		{name: "device_repository", present: cont.DeviceRepo != nil},
		{name: "rule_repository", present: cont.RuleRepo != nil},
		{name: "alarm_repository", present: cont.AlarmRepo != nil},
		{name: "time_series_storage", present: cont.TsStorage != nil},
		{name: "system_service", present: cont.SystemService != nil},
		{name: "scheduler", present: cont.Scheduler != nil},
	} {
		if !dep.present {
			missing = append(missing, dep.name)
		}
	}
	status := "healthy"
	if len(missing) > 0 {
		status = "degraded"
	}
	result := map[string]interface{}{
		"status":  status,
		"version": gatewayVersion,
	}
	if len(missing) > 0 {
		result["missing_components"] = missing
		result["note"] = "counters are omitted for the components that are missing; an absent counter means the gateway cannot see it, not zero"
	}
	if cont.SystemService != nil {
		result["uptime_s"] = cont.SystemService.GetSystemInfo()["uptime_s"]
	}
	if cont.DeviceRepo != nil {
		devices, _, err := cont.DeviceRepo.List(1, mcpMaxLimit)
		if err != nil {
			return nil, newMCPToolError("ERR_MCP_CALL_FAILED", "counting devices failed: %v", err)
		}
		online := 0
		for _, d := range devices {
			if d.Status == "online" {
				online++
			}
		}
		result["device_total"] = len(devices)
		result["device_online"] = online
	}
	if cont.RuleRepo != nil {
		rules, _, err := cont.RuleRepo.List(1, mcpMaxLimit, "")
		if err != nil {
			return nil, newMCPToolError("ERR_MCP_CALL_FAILED", "counting rules failed: %v", err)
		}
		enabled := 0
		for _, r := range rules {
			if r.Enabled {
				enabled++
			}
		}
		result["rule_total"] = len(rules)
		result["rule_enabled"] = enabled
	}
	if cont.AlarmRepo != nil {
		firing, _, err := cont.AlarmRepo.List(models.AlarmFilter{Status: "firing"}, 1, mcpMaxLimit)
		if err != nil {
			return nil, newMCPToolError("ERR_MCP_CALL_FAILED", "counting alarms failed: %v", err)
		}
		result["alarm_firing"] = len(firing)
	}
	if cont.Scheduler != nil {
		result["scheduler"] = cont.Scheduler.Stats()
	}
	return result, nil
}

func mcpEmptyNote(prefix, detail string) string {
	if strings.TrimSpace(detail) == "" {
		return prefix
	}
	return prefix + " for the filter that was sent"
}
