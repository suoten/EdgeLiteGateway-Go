package drivers

import (
	"fmt"
	"math"
	"strings"

	"github.com/sirupsen/logrus"
)

// EdgeTriggerExecutor executes actions from edge rule alarms.
// It handles write, MQTT publish, and webhook actions.
type EdgeTriggerExecutor struct {
	writeFn   func(deviceID, point string, value interface{}) error
	mqttFn    func(topic string, payload []byte) error
	webhookFn func(url string, payload []byte) error
}

// NewEdgeTriggerExecutor creates a new EdgeTriggerExecutor.
func NewEdgeTriggerExecutor() *EdgeTriggerExecutor {
	return &EdgeTriggerExecutor{}
}

// SetWriteHandler sets the handler for write actions.
func (e *EdgeTriggerExecutor) SetWriteHandler(fn func(deviceID, point string, value interface{}) error) {
	e.writeFn = fn
}

// SetMQTTPublisher sets the handler for MQTT publish actions.
func (e *EdgeTriggerExecutor) SetMQTTPublisher(fn func(topic string, payload []byte) error) {
	e.mqttFn = fn
}

// SetWebhookSender sets the handler for webhook actions.
func (e *EdgeTriggerExecutor) SetWebhookSender(fn func(url string, payload []byte) error) {
	e.webhookFn = fn
}

// ExecuteActions executes all actions for a given alarm record.
func (e *EdgeTriggerExecutor) ExecuteActions(alarm *AlarmRecord, rule *EdgeRule) {
	if rule == nil || len(rule.Actions) == 0 {
		return
	}

	for _, action := range rule.Actions {
		if err := e.executeAction(action, alarm); err != nil {
			logrus.WithError(err).WithFields(logrus.Fields{
				"rule_id":     rule.RuleID,
				"action_type": action.Type,
			}).Warn("Edge trigger action failed")
		}
	}
}

// executeAction executes a single action.
func (e *EdgeTriggerExecutor) executeAction(action EdgeRuleAction, alarm *AlarmRecord) error {
	switch action.Type {
	case "write":
		return e.executeWriteAction(action, alarm)
	case "mqtt_publish":
		return e.executeMQTTPublishAction(action, alarm)
	case "webhook":
		return e.executeWebhookAction(action, alarm)
	case "log":
		logrus.WithFields(logrus.Fields{
			"alarm":  alarm.ToDict(),
			"params": action.Params,
		}).Info("Edge trigger log action")
		return nil
	default:
		return fmt.Errorf("unknown action type: %s", action.Type)
	}
}

// executeWriteAction executes a write action to a device.
func (e *EdgeTriggerExecutor) executeWriteAction(action EdgeRuleAction, alarm *AlarmRecord) error {
	if e.writeFn == nil {
		return fmt.Errorf("write handler not set")
	}
	target := action.Target
	if target == "" {
		target = alarm.DeviceID
	}
	point, _ := action.Params["point"].(string)
	value := action.Params["value"]
	if point == "" {
		return fmt.Errorf("write action missing 'point' parameter")
	}
	return e.writeFn(target, point, value)
}

// executeMQTTPublishAction executes an MQTT publish action.
func (e *EdgeTriggerExecutor) executeMQTTPublishAction(action EdgeRuleAction, alarm *AlarmRecord) error {
	if e.mqttFn == nil {
		return fmt.Errorf("MQTT publisher not set")
	}
	topic := action.Target
	if topic == "" {
		topic = fmt.Sprintf("edgelite/alarms/%s", alarm.DeviceID)
	}
	payload := []byte(fmt.Sprintf(`{"rule_id":"%s","device_id":"%s","point":"%s","value":%f,"severity":"%s","message":"%s"}`,
		alarm.RuleID, alarm.DeviceID, alarm.PointName, alarm.Value, alarm.Severity, alarm.Message))
	return e.mqttFn(topic, payload)
}

// executeWebhookAction executes a webhook POST action.
func (e *EdgeTriggerExecutor) executeWebhookAction(action EdgeRuleAction, alarm *AlarmRecord) error {
	if e.webhookFn == nil {
		return fmt.Errorf("webhook sender not set")
	}
	url := action.Target
	if url == "" {
		return fmt.Errorf("webhook action missing URL")
	}
	payload := []byte(fmt.Sprintf(`{"rule_id":"%s","device_id":"%s","point":"%s","value":%f,"severity":"%s","timestamp":"%s"}`,
		alarm.RuleID, alarm.DeviceID, alarm.PointName, alarm.Value, alarm.Severity, alarm.Timestamp.Format("2006-01-02T15:04:05Z07:00")))
	return e.webhookFn(url, payload)
}

// SafeEvalExpr provides safe expression evaluation for simulator formulas.
// It evaluates mathematical expressions without using eval().
// Supported: +, -, *, /, %, ^, parentheses, math functions (sin, cos, sqrt, etc.)
func SafeEvalExpr(expr string, vars map[string]float64) (float64, error) {
	// This is a simplified expression evaluator.
	// In production, use a proper expression parser library.
	tokens := tokenizeExpr(expr)
	result, _, err := parseExprTokens(tokens, 0, vars)
	return result, err
}

// tokenizeExpr tokenizes a mathematical expression string.
func tokenizeExpr(expr string) []string {
	var tokens []string
	i := 0
	for i < len(expr) {
		c := expr[i]
		if c == ' ' || c == '\t' || c == '\n' {
			i++
			continue
		}
		if c >= '0' && c <= '9' || c == '.' {
			start := i
			for i < len(expr) && (expr[i] >= '0' && expr[i] <= '9' || expr[i] == '.') {
				i++
			}
			tokens = append(tokens, expr[start:i])
			continue
		}
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' {
			start := i
			for i < len(expr) && (expr[i] >= 'a' && expr[i] <= 'z' || expr[i] >= 'A' && expr[i] <= 'Z' || expr[i] == '_' || expr[i] >= '0' && expr[i] <= '9') {
				i++
			}
			tokens = append(tokens, expr[start:i])
			continue
		}
		if strings.Contains("+-*/%^()", string(c)) {
			tokens = append(tokens, string(c))
			i++
			continue
		}
		i++ // Skip unknown chars
	}
	return tokens
}

// parseExprTokens parses tokens starting at position pos and returns the result.
// This is a recursive descent parser for mathematical expressions.
func parseExprTokens(tokens []string, pos int, vars map[string]float64) (float64, int, error) {
	return parseAddSub(tokens, pos, vars)
}

func parseAddSub(tokens []string, pos int, vars map[string]float64) (float64, int, error) {
	left, pos, err := parseMulDiv(tokens, pos, vars)
	if err != nil {
		return 0, pos, err
	}
	for pos < len(tokens) {
		op := tokens[pos]
		if op != "+" && op != "-" {
			break
		}
		pos++
		right, newPos, err := parseMulDiv(tokens, pos, vars)
		if err != nil {
			return 0, newPos, err
		}
		pos = newPos
		if op == "+" {
			left += right
		} else {
			left -= right
		}
	}
	return left, pos, nil
}

func parseMulDiv(tokens []string, pos int, vars map[string]float64) (float64, int, error) {
	left, pos, err := parsePower(tokens, pos, vars)
	if err != nil {
		return 0, pos, err
	}
	for pos < len(tokens) {
		op := tokens[pos]
		if op != "*" && op != "/" && op != "%" {
			break
		}
		pos++
		right, newPos, err := parsePower(tokens, pos, vars)
		if err != nil {
			return 0, newPos, err
		}
		pos = newPos
		switch op {
		case "*":
			left *= right
		case "/":
			if right == 0 {
				return 0, pos, fmt.Errorf("division by zero")
			}
			left /= right
		case "%":
			left = math.Mod(left, right)
		}
	}
	return left, pos, nil
}

func parsePower(tokens []string, pos int, vars map[string]float64) (float64, int, error) {
	left, pos, err := parsePrimary(tokens, pos, vars)
	if err != nil {
		return 0, pos, err
	}
	for pos < len(tokens) && tokens[pos] == "^" {
		pos++
		right, newPos, err := parsePrimary(tokens, pos, vars)
		if err != nil {
			return 0, newPos, err
		}
		pos = newPos
		left = math.Pow(left, right)
	}
	return left, pos, nil
}

func parsePrimary(tokens []string, pos int, vars map[string]float64) (float64, int, error) {
	if pos >= len(tokens) {
		return 0, pos, fmt.Errorf("unexpected end of expression")
	}
	token := tokens[pos]

	// Parentheses
	if token == "(" {
		pos++
		result, newPos, err := parseAddSub(tokens, pos, vars)
		if err != nil {
			return 0, newPos, err
		}
		pos = newPos
		if pos >= len(tokens) || tokens[pos] != ")" {
			return 0, pos, fmt.Errorf("expected closing parenthesis")
		}
		pos++
		return result, pos, nil
	}

	// Unary minus
	if token == "-" {
		pos++
		result, newPos, err := parsePrimary(tokens, pos, vars)
		return -result, newPos, err
	}

	// Number
	if token[0] >= '0' && token[0] <= '9' || token[0] == '.' {
		var num float64
		_, _ = fmt.Sscanf(token, "%f", &num)
		return num, pos + 1, nil
	}

	// Variable or function
	if token[0] >= 'a' && token[0] <= 'z' || token[0] >= 'A' && token[0] <= 'Z' || token[0] == '_' {
		// Check if next token is "(" (function call)
		if pos+1 < len(tokens) && tokens[pos+1] == "(" {
			// Parse function call
			pos += 2 // Skip name and "("
			arg, newPos, err := parseAddSub(tokens, pos, vars)
			if err != nil {
				return 0, newPos, err
			}
			pos = newPos
			if pos >= len(tokens) || tokens[pos] != ")" {
				return 0, pos, fmt.Errorf("expected closing parenthesis for function %s", token)
			}
			return evalMathFunc(token, arg)
		}
		// Variable
		if val, ok := vars[token]; ok {
			return val, pos + 1, nil
		}
		// Constants
		switch token {
		case "pi", "PI":
			return math.Pi, pos + 1, nil
		case "e", "E":
			return math.E, pos + 1, nil
		}
		return 0, pos, fmt.Errorf("undefined variable: %s", token)
	}

	return 0, pos, fmt.Errorf("unexpected token: %s", token)
}

// evalMathFunc evaluates a math function by name.
func evalMathFunc(name string, arg float64) (float64, int, error) {
	switch name {
	case "abs":
		return math.Abs(arg), 0, nil
	case "sin":
		return math.Sin(arg), 0, nil
	case "cos":
		return math.Cos(arg), 0, nil
	case "tan":
		return math.Tan(arg), 0, nil
	case "sqrt":
		return math.Sqrt(arg), 0, nil
	case "log":
		return math.Log(arg), 0, nil
	case "log10":
		return math.Log10(arg), 0, nil
	case "exp":
		return math.Exp(arg), 0, nil
	case "floor":
		return math.Floor(arg), 0, nil
	case "ceil":
		return math.Ceil(arg), 0, nil
	case "round":
		return math.Round(arg), 0, nil
	default:
		return 0, 0, fmt.Errorf("unknown function: %s", name)
	}
}
