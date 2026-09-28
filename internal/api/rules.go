package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"

	"edgelite/internal/constants"
	"edgelite/internal/models"
	"edgelite/internal/security"
	"edgelite/internal/storage"
)

// RegisterRuleRoutes registers rule management API routes.
func RegisterRuleRoutes(g *echo.Group) {
	g.GET("", handleListRules, requirePermission(security.PermRuleRead))
	g.POST("", handleCreateRule, requirePermission(security.PermRuleCreate))
	g.POST("/test", handleTestRule, requirePermission(security.PermRuleRead))
	g.POST("/batch/delete", handleBatchDeleteRules, requirePermission(security.PermRuleDelete))
	g.POST("/batch/enable", handleBatchEnableRules, requirePermission(security.PermRuleUpdate))
	g.POST("/batch/disable", handleBatchDisableRules, requirePermission(security.PermRuleUpdate))
	g.GET("/:rule_id", handleGetRule, requirePermission(security.PermRuleRead))
	g.PUT("/:rule_id", handleUpdateRule, requirePermission(security.PermRuleUpdate))
	g.DELETE("/:rule_id", handleDeleteRule, requirePermission(security.PermRuleDelete))
	g.POST("/:rule_id/enable", handleEnableRule, requirePermission(security.PermRuleUpdate))
	g.POST("/:rule_id/disable", handleDisableRule, requirePermission(security.PermRuleUpdate))
	g.POST("/:rule_id/test", handleTestRuleByID, requirePermission(security.PermRuleRead))
	g.GET("/:rule_id/versions", handleListRuleVersions, requirePermission(security.PermRuleRead))
	g.GET("/:rule_id/versions/:version", handleGetRuleVersion, requirePermission(security.PermRuleRead))
	g.POST("/:rule_id/versions/rollback", handleRollbackRuleVersion, requirePermission(security.PermRuleUpdate))
}

func handleListRules(c echo.Context) error {
	user := getUserFromContext(c)
	page, size := parsePagination(c)
	deviceID := c.QueryParam("device_id")
	search := c.QueryParam("search")
	severity := c.QueryParam("severity")

	if page <= 0 {
		page = 1
	}
	if size <= 0 {
		size = 20
	}

	cont := GetContainer()
	if cont.RuleService == nil {
		return ServiceUnavailable(c, "Rule service not ready")
	}

	// Same reason as the device list: the repo pages before severity/search/
	// ownership can be applied, so a filter needs the full set first.
	needFullScan := severity != "" || search != "" || user.Role != security.RoleAdmin

	var rules []models.RuleResponse
	var total int
	var err error
	if needFullScan {
		rules, _, err = cont.RuleRepo.List(1, constants.MaxQuerySize, deviceID)
	} else {
		rules, total, err = cont.RuleService.List(page, size, deviceID)
	}
	if err != nil {
		return InternalError(c, "ERR_RULE_LIST_FAILED")
	}

	// Filter by severity/search (in-memory)
	if severity != "" || search != "" {
		filtered := rules[:0]
		for _, r := range rules {
			if severity != "" && r.Severity != severity {
				continue
			}
			if search != "" && !containsInsensitive(r.Name, search) {
				continue
			}
			filtered = append(filtered, r)
		}
		rules = filtered
	}

	// Filter by ownership
	if user.Role != security.RoleAdmin {
		filtered := rules[:0]
		for _, r := range rules {
			if r.CreatedBy == user.UserID {
				filtered = append(filtered, r)
			}
		}
		rules = filtered
	}

	if needFullScan {
		total = len(rules)
		rules = paginateSlice(rules, page, size)
	}

	if rules == nil {
		rules = []models.RuleResponse{}
	}
	return OKPaged(c, rules, total, page, size)
}

func handleCreateRule(c echo.Context) error {
	user := getUserFromContext(c)
	var req models.RuleCreate
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}

	if req.Name == "" {
		return BadRequest(c, "ERR_RULE_CONDITION_INVALID: name required")
	}
	if len(req.Conditions) == 0 {
		return BadRequest(c, "ERR_RULE_CONDITION_INVALID: conditions required")
	}

	cont := GetContainer()
	rule, err := cont.RuleService.Create(&req, user.UserID)
	if err != nil {
		logrus.WithError(err).Error("Create rule failed")
		return BadRequest(c, "ERR_RULE_CONDITION_INVALID")
	}

	recordAudit(c, "rule_create", "rule", rule.RuleID, "success", map[string]interface{}{"name": req.Name, "device_id": req.DeviceID})

	return Created(c, rule)
}

func handleTestRule(c echo.Context) error {
	var req models.RuleCreate
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}

	conditions := make([]map[string]interface{}, len(req.Conditions))
	for i, cond := range req.Conditions {
		conditions[i] = map[string]interface{}{
			"point":     cond.Point,
			"operator":  cond.Operator,
			"threshold": cond.Threshold,
			"type":      cond.Type,
		}
	}

	return OK(c, map[string]interface{}{
		"rule_name":        req.Name,
		"device_id":        req.DeviceID,
		"severity":         req.Severity,
		"logic":            req.Logic,
		"conditions":       conditions,
		"duration":         req.Duration,
		"notify_channels":  req.NotifyChannels,
		"evaluable":        true,
	})
}

func handleBatchDeleteRules(c echo.Context) error {
	user := getUserFromContext(c)
	var req BatchRuleIDs
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}

	if len(req.RuleIDs) == 0 {
		return BadRequest(c, "rule_ids required")
	}
	if len(req.RuleIDs) > maxBatchSize {
		return BadRequest(c, "Batch size exceeds limit")
	}

	cont := GetContainer()
	if cont.RuleService == nil {
		return ServiceUnavailable(c, "Rule service not ready")
	}
	successCount := 0
	failed := make(map[string]string)
	for _, id := range req.RuleIDs {
		rule, err := cont.RuleService.Get(id)
		if err != nil {
			failed[id] = "ERR_RULE_GET_FAILED"
			continue
		}
		// A rule that is not there cannot be deleted; counting it among the
		// successes told the operator that rows were removed when none were.
		if rule == nil {
			failed[id] = "ERR_RULE_NOT_FOUND"
			continue
		}
		if user.Role != security.RoleAdmin && rule.CreatedBy != user.UserID {
			failed[id] = "ERR_AUTHZ_RESOURCE_OWNERSHIP_DENIED"
			continue
		}
		if err := cont.RuleService.Delete(id); err != nil {
			failed[id] = "ERR_RULE_DELETE_FAILED"
		} else {
			successCount++
		}
	}

	if successCount > 0 {
		recordAudit(c, "rule_delete", "rule", "", "success", map[string]interface{}{"batch": true, "deleted": successCount, "failed": len(failed)})
	}

	return OK(c, map[string]interface{}{
		"success_count": successCount,
		"failed":        failed,
	})
}

// BatchRuleIDs is the {"rule_ids": [...]} body shared by the batch rule routes.
type BatchRuleIDs struct {
	RuleIDs []string `json:"rule_ids"`
}

func handleBatchEnableRules(c echo.Context) error {
	return batchSetRulesEnabled(c, true)
}

func handleBatchDisableRules(c echo.Context) error {
	return batchSetRulesEnabled(c, false)
}

// batchSetRulesEnabled toggles each named rule and reports per ID what actually
// happened. It used to answer success_count for IDs that matched no row, and to
// blame every failure on a missing rule.
func batchSetRulesEnabled(c echo.Context, enabled bool) error {
	user := getUserFromContext(c)
	var req BatchRuleIDs
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	if len(req.RuleIDs) == 0 {
		return BadRequest(c, "rule_ids required")
	}
	if len(req.RuleIDs) > maxBatchSize {
		return BadRequest(c, "Batch size exceeds limit")
	}

	cont := GetContainer()
	if cont.RuleService == nil {
		return ServiceUnavailable(c, "Rule service not ready")
	}

	successCount := 0
	failed := make(map[string]string)
	for _, id := range req.RuleIDs {
		rule, err := cont.RuleService.Get(id)
		if err != nil {
			failed[id] = "ERR_RULE_GET_FAILED"
			continue
		}
		if rule == nil {
			failed[id] = "ERR_RULE_NOT_FOUND"
			continue
		}
		if user.Role != security.RoleAdmin && rule.CreatedBy != user.UserID {
			failed[id] = "ERR_AUTHZ_RESOURCE_OWNERSHIP_DENIED"
			continue
		}
		if err := cont.RuleService.SetEnabled(id, enabled); err != nil {
			failed[id] = ruleToggleFailure(err, enabled)
		} else {
			successCount++
		}
	}

	action := "rule_disable"
	if enabled {
		action = "rule_enable"
	}
	if successCount > 0 {
		recordAudit(c, action, "rule", "", "success", map[string]interface{}{"batch": true, "changed": successCount, "failed": len(failed)})
	}

	return OK(c, map[string]interface{}{
		"success_count": successCount,
		"failed":        failed,
	})
}

// ruleToggleFailure labels a toggle failure by what the store actually
// reported: a rule that vanished between the check and the write is
// ERR_RULE_NOT_FOUND, anything else is the store refusing the write.
func ruleToggleFailure(err error, enabled bool) string {
	if errors.Is(err, storage.ErrRuleNotFound) {
		return "ERR_RULE_NOT_FOUND"
	}
	if enabled {
		return "ERR_RULE_ENABLE_FAILED"
	}
	return "ERR_RULE_DISABLE_FAILED"
}

func handleGetRule(c echo.Context) error {
	user := getUserFromContext(c)
	ruleID := c.Param("rule_id")
	if ruleID == "" {
		return BadRequest(c, "Rule ID required")
	}

	cont := GetContainer()
	rule, err := cont.RuleService.Get(ruleID)
	if err != nil || rule == nil {
		return NotFound(c, "ERR_RULE_NOT_FOUND")
	}

	if user.Role != security.RoleAdmin && rule.CreatedBy != user.UserID {
		return Forbidden(c, "ERR_AUTHZ_RESOURCE_OWNERSHIP_DENIED")
	}

	return OK(c, rule)
}

func handleUpdateRule(c echo.Context) error {
	user := getUserFromContext(c)
	ruleID := c.Param("rule_id")
	if ruleID == "" {
		return BadRequest(c, "Rule ID required")
	}

	var req models.RuleUpdate
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}

	cont := GetContainer()
	existing, _ := cont.RuleService.Get(ruleID)
	if existing == nil {
		return NotFound(c, "ERR_RULE_NOT_FOUND")
	}
	if user.Role != security.RoleAdmin && existing.CreatedBy != user.UserID {
		return Forbidden(c, "ERR_AUTHZ_RESOURCE_OWNERSHIP_DENIED")
	}

	rule, err := cont.RuleService.Update(ruleID, &req)
	if err != nil {
		return InternalError(c, "ERR_RULE_UPDATE_FAILED")
	}
	recordRuleVersion(cont, ruleID, rule, user.Username)

	recordAudit(c, "rule_update", "rule", ruleID, "success", nil)

	return OK(c, rule)
}

// ruleVersion keys live in the settings store so the existing sqlite plumbing
// (ListSettingKeys/GetSetting/SetSetting) serves list/get/rollback without a
// second schema path; the rule_versions table created in database.go is not
// wired to any repository and stayed empty.
func ruleVersionNextKey(ruleID string) string { return "rule_version_next:" + ruleID }
func ruleVersionKey(ruleID string, v int) string {
	return fmt.Sprintf("rule_version:%s:%d", ruleID, v)
}

func recordRuleVersion(cont *ServiceContainer, ruleID string, rule *models.RuleResponse, username string) {
	if cont.Database == nil || rule == nil {
		return
	}
	next := 1
	if raw, err := cont.Database.GetSetting(ruleVersionNextKey(ruleID)); err == nil {
		if n, convErr := strconv.Atoi(strings.TrimSpace(raw)); convErr == nil && n >= next {
			next = n + 1
		}
	}
	doc := map[string]interface{}{
		"version":     next,
		"rule":        rule,
		"change":      "update",
		"created_by":  username,
		"created_at":  time.Now().Format(time.RFC3339),
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return
	}
	if err := cont.Database.SetSetting(ruleVersionKey(ruleID, next), string(raw)); err != nil {
		logrus.WithError(err).WithField("rule_id", ruleID).Warn("Failed to record rule version")
		return
	}
	_ = cont.Database.SetSetting(ruleVersionNextKey(ruleID), strconv.Itoa(next))
}

func handleDeleteRule(c echo.Context) error {
	user := getUserFromContext(c)
	ruleID := c.Param("rule_id")
	if ruleID == "" {
		return BadRequest(c, "Rule ID required")
	}

	cont := GetContainer()
	existing, _ := cont.RuleService.Get(ruleID)
	if existing == nil {
		return NotFound(c, "ERR_RULE_NOT_FOUND")
	}
	if user.Role != security.RoleAdmin && existing.CreatedBy != user.UserID {
		return Forbidden(c, "ERR_AUTHZ_RESOURCE_OWNERSHIP_DENIED")
	}

	if err := cont.RuleService.Delete(ruleID); err != nil {
		return InternalError(c, "ERR_RULE_DELETE_FAILED")
	}

	recordAudit(c, "rule_delete", "rule", ruleID, "success", nil)

	return OK(c, nil)
}

func handleEnableRule(c echo.Context) error {
	return setRuleEnabled(c, true)
}

func handleDisableRule(c echo.Context) error {
	return setRuleEnabled(c, false)
}

// setRuleEnabled toggles one rule. Before, the repo's UPDATE matched zero rows
// without complaining and every route answered 200, so enabling a rule that did
// not exist looked like it worked; any real store error was reported as 404.
func setRuleEnabled(c echo.Context, enabled bool) error {
	user := getUserFromContext(c)
	ruleID := c.Param("rule_id")
	if ruleID == "" {
		return BadRequest(c, "Rule ID required")
	}

	cont := GetContainer()
	if cont.RuleService == nil {
		return ServiceUnavailable(c, "Rule service not ready")
	}
	existing, err := cont.RuleService.Get(ruleID)
	if err != nil {
		return InternalError(c, "ERR_RULE_READ_FAILED")
	}
	if existing == nil {
		return NotFound(c, "ERR_RULE_NOT_FOUND")
	}
	if user.Role != security.RoleAdmin && existing.CreatedBy != user.UserID {
		return Forbidden(c, "ERR_AUTHZ_RESOURCE_OWNERSHIP_DENIED")
	}

	if err := cont.RuleService.SetEnabled(ruleID, enabled); err != nil {
		if errors.Is(err, storage.ErrRuleNotFound) {
			return NotFound(c, "ERR_RULE_NOT_FOUND")
		}
		logrus.WithError(err).Error("Toggle rule failed")
		return InternalError(c, ruleToggleFailure(err, enabled))
	}

	action := "rule_disable"
	if enabled {
		action = "rule_enable"
	}
	recordAudit(c, action, "rule", ruleID, "success", nil)

	rule, _ := cont.RuleService.Get(ruleID)
	return OK(c, rule)
}

func handleTestRuleByID(c echo.Context) error {
	ruleID := c.Param("rule_id")
	if ruleID == "" {
		return BadRequest(c, "Rule ID required")
	}

	var req models.RuleTestRequest
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}

	cont := GetContainer()
	rule, _ := cont.RuleService.Get(ruleID)
	if rule == nil {
		return NotFound(c, "ERR_RULE_NOT_FOUND")
	}

	triggered, triggerValues, err := cont.RuleService.TestRule(&req, rule.Conditions, rule.Logic)
	if err != nil {
		return BadRequest(c, "ERR_RULE_CONDITION_INVALID")
	}

	return OK(c, map[string]interface{}{
		"triggered":      triggered,
		"trigger_values":  triggerValues,
		"rule_id":         ruleID,
	})
}

// handleListRuleVersions returns version history for a rule.
func handleListRuleVersions(c echo.Context) error {
	ruleID := c.Param("rule_id")
	if ruleID == "" {
		return BadRequest(c, "Rule ID required")
	}
	cont := GetContainer()
	if cont.Database == nil {
		return ServiceUnavailable(c, "ERR_COMMON_DB_NOT_READY")
	}
	keys, err := cont.Database.ListSettingKeys("rule_version:" + ruleID + ":")
	if err != nil {
		return InternalError(c, "ERR_RULE_VERSION_LIST_FAILED")
	}
	out := make([]map[string]interface{}, 0, len(keys))
	for _, key := range keys {
		raw, err := cont.Database.GetSetting(key)
		if err != nil {
			continue
		}
		var doc map[string]interface{}
		if json.Unmarshal([]byte(raw), &doc) != nil {
			continue
		}
		out = append(out, map[string]interface{}{
			"version":    doc["version"],
			"updated_at": doc["created_at"],
			"updated_by": doc["created_by"],
			"change":     doc["change"],
		})
	}
	sort.Slice(out, func(i, j int) bool {
		vi, _ := out[i]["version"].(float64)
		vj, _ := out[j]["version"].(float64)
		return vi > vj
	})
	return OK(c, out)
}

// handleGetRuleVersion returns a specific version of a rule.
func handleGetRuleVersion(c echo.Context) error {
	ruleID := c.Param("rule_id")
	version := c.Param("version")
	if ruleID == "" || version == "" {
		return BadRequest(c, "Rule ID and version required")
	}
	cont := GetContainer()
	if cont.Database == nil {
		return ServiceUnavailable(c, "ERR_COMMON_DB_NOT_READY")
	}
	raw, err := cont.Database.GetSetting(ruleVersionKey(ruleID, atoiOrZero(version)))
	if err != nil || raw == "" {
		return NotFound(c, "ERR_RULE_VERSION_NOT_FOUND")
	}
	var doc map[string]interface{}
	if json.Unmarshal([]byte(raw), &doc) != nil {
		return InternalError(c, "ERR_RULE_VERSION_CORRUPT")
	}
	return OK(c, doc)
}

// handleRollbackRuleVersion rolls back a rule to a specific version.
func handleRollbackRuleVersion(c echo.Context) error {
	ruleID := c.Param("rule_id")
	if ruleID == "" {
		return BadRequest(c, "Rule ID required")
	}
	var req struct {
		Version int `json:"version"`
	}
	if err := c.Bind(&req); err != nil {
		return BadRequest(c, "Invalid request body")
	}
	cont := GetContainer()
	if cont.Database == nil {
		return ServiceUnavailable(c, "ERR_COMMON_DB_NOT_READY")
	}
	raw, err := cont.Database.GetSetting(ruleVersionKey(ruleID, req.Version))
	if err != nil || raw == "" {
		return NotFound(c, "ERR_RULE_VERSION_NOT_FOUND")
	}
	var doc struct {
		Rule *models.RuleResponse `json:"rule"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil || doc.Rule == nil {
		return InternalError(c, "ERR_RULE_VERSION_CORRUPT")
	}
	// Apply the snapshot through the standard update path so evaluators and
	// audit stay consistent; the rollback itself is then recorded as a new
	// version, which makes roll-forward possible.
	cond := doc.Rule.Conditions
	threshold := 0.0
	if len(cond) > 0 {
		threshold = cond[0].Threshold
	}
	rule, err := cont.RuleService.Update(ruleID, &models.RuleUpdate{Conditions: &cond})
	_ = threshold
	if err != nil {
		return InternalError(c, "ERR_RULE_UPDATE_FAILED")
	}
	if user := getUserFromContext(c); user != nil {
		recordRuleVersion(cont, ruleID, rule, user.Username)
	}
	recordAudit(c, "rule_rollback", "rule", ruleID, "success", map[string]interface{}{"rolled_back_to": req.Version})
	return OK(c, map[string]interface{}{
		"rule_id":        ruleID,
		"rolled_back_to": req.Version,
		"rule":           rule,
	})
}

func atoiOrZero(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}
