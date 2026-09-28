package engine

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// ExpressionEngine provides a safe expression evaluation sandbox.
// This is a Go port of the Python edgelite/engine/expression_engine.py.
//
// Supported features:
//   - Arithmetic: +, -, *, /, %, **
//   - Comparison: ==, !=, >, <, >=, <=
//   - Logical: and, or, not
//   - Math functions: abs, round, min, max, pow, sqrt, ceil, floor, log, log10
//   - Type conversion: int, float, str, bool
//   - Variable references: ${device_id.point_name}
//
// Unlike the Python version which uses Python's ast module, the Go version
// implements a custom recursive descent parser for safety - no Go runtime
// introspection is exposed.

// ExpressionEngine evaluates mathematical and logical expressions safely.
type ExpressionEngine struct {
	mu               sync.RWMutex
	customFunctions  map[string]ExpressionFunc
	evalTimeout      time.Duration
}

// ExpressionFunc is a custom function that can be registered with the engine.
type ExpressionFunc func(args []interface{}) (interface{}, error)

// NewExpressionEngine creates a new ExpressionEngine.
func NewExpressionEngine() *ExpressionEngine {
	return &ExpressionEngine{
		customFunctions: make(map[string]ExpressionFunc),
		evalTimeout:     5 * time.Second,
	}
}

// RegisterFunction registers a custom function that can be used in expressions.
func (e *ExpressionEngine) RegisterFunction(name string, fn ExpressionFunc) error {
	if isDangerousName(name) {
		return fmt.Errorf("cannot register dangerous name: %s", name)
	}
	if strings.HasPrefix(name, "__") && strings.HasSuffix(name, "__") {
		return fmt.Errorf("cannot register dunder name: %s", name)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.customFunctions[name] = fn
	return nil
}

// Evaluate evaluates an expression string with the given variables.
// Variables can be referenced via ${device_id.point_name} syntax.
func (e *ExpressionEngine) Evaluate(expression string, variables map[string]interface{}) interface{} {
	result, err := e.EvaluateDetailed(expression, variables)
	if err != nil {
		logrus.WithError(err).WithField("expression", expression).Debug("Expression evaluation failed")
		return nil
	}
	return result
}

// EvaluateDetailed is Evaluate with the reason attached. The workbench needs it:
// a swallowed error is how the test page could answer "null" for a typo in a
// variable name and for a division by zero, which are two different things to
// whoever is writing the expression.
func (e *ExpressionEngine) EvaluateDetailed(expression string, variables map[string]interface{}) (interface{}, error) {
	if strings.TrimSpace(expression) == "" {
		return nil, errors.New("expression is empty")
	}

	// Resolve variable references
	resolved, err := e.resolveVariables(expression, variables)
	if err != nil {
		return nil, err
	}

	return e.evalExpr(resolved)
}

// EvaluateBatch evaluates multiple expressions at once.
func (e *ExpressionEngine) EvaluateBatch(expressions map[string]string, variables map[string]interface{}) map[string]interface{} {
	results := make(map[string]interface{})
	for name, expr := range expressions {
		results[name] = e.Evaluate(expr, variables)
	}
	return results
}

// ValidateExpression validates that an expression is safe to evaluate.
func (e *ExpressionEngine) ValidateExpression(expression string) error {
	if expression == "" || strings.TrimSpace(expression) == "" {
		return nil
	}
	resolved := expression // Don't resolve variables for validation
	_, err := e.evalExpr(resolved)
	return err
}

var variablePattern = regexp.MustCompile(`\$\{([^}]+)\}`)

func (e *ExpressionEngine) resolveVariables(expression string, variables map[string]interface{}) (string, error) {
	var missingVars []string

	resolved := variablePattern.ReplaceAllStringFunc(expression, func(match string) string {
		// Extract variable name from ${...}
		varPath := match[2 : len(match)-1]
		value, exists := variables[varPath]
		if !exists {
			// Try without dot notation
			parts := strings.SplitN(varPath, ".", 2)
			if len(parts) == 2 {
				key := parts[0] + "." + parts[1]
				value, exists = variables[key]
			}
		}
		if !exists {
			missingVars = append(missingVars, varPath)
			return "0"
		}
		return formatValueForExpr(value)
	})

	if len(missingVars) > 0 {
		return "", fmt.Errorf("undefined variables: %s", strings.Join(missingVars, ", "))
	}
	return resolved, nil
}

func formatValueForExpr(v interface{}) string {
	switch val := v.(type) {
	case bool:
		if val {
			return "1"
		}
		return "0"
	case int, int64, float64:
		return fmt.Sprintf("%v", val)
	case string:
		return val
	default:
		return fmt.Sprintf("%v", val)
	}
}

// evalExpr is a simple recursive descent parser/evaluator for safe expressions.
// It supports basic arithmetic, comparison, and logical operations.
func (e *ExpressionEngine) evalExpr(expr string) (interface{}, error) {
	parser := &exprParser{
		engine: e,
		input:  strings.TrimSpace(expr),
		pos:    0,
	}
	return parser.parseExpression()
}

// exprParser is a recursive descent parser for safe expression evaluation.
type exprParser struct {
	engine *ExpressionEngine
	input  string
	pos   int
}

func (p *exprParser) peek() byte {
	if p.pos >= len(p.input) {
		return 0
	}
	return p.input[p.pos]
}

func (p *exprParser) skipWhitespace() {
	for p.pos < len(p.input) && (p.input[p.pos] == ' ' || p.input[p.pos] == '\t') {
		p.pos++
	}
}

func (p *exprParser) parseExpression() (interface{}, error) {
	return p.parseOr()
}

func (p *exprParser) parseOr() (interface{}, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for {
		p.skipWhitespace()
		if p.matchKeyword("or") {
			right, err := p.parseAnd()
			if err != nil {
				return nil, err
			}
			left = toBool(left) || toBool(right)
		} else {
			break
		}
	}
	return left, nil
}

func (p *exprParser) parseAnd() (interface{}, error) {
	left, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	for {
		p.skipWhitespace()
		if p.matchKeyword("and") {
			right, err := p.parseNot()
			if err != nil {
				return nil, err
			}
			left = toBool(left) && toBool(right)
		} else {
			break
		}
	}
	return left, nil
}

func (p *exprParser) parseNot() (interface{}, error) {
	p.skipWhitespace()
	if p.matchKeyword("not") {
		val, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		return !toBool(val), nil
	}
	return p.parseComparison()
}

func (p *exprParser) parseComparison() (interface{}, error) {
	left, err := p.parseAddSub()
	if err != nil {
		return nil, err
	}
	for {
		p.skipWhitespace()
		var op string
		if p.matchString(">=") {
			op = ">="
		} else if p.matchString("<=") {
			op = "<="
		} else if p.matchString("==") {
			op = "=="
		} else if p.matchString("!=") {
			op = "!="
		} else if p.matchString(">") {
			op = ">"
		} else if p.matchString("<") {
			op = "<"
		} else {
			break
		}
		right, err := p.parseAddSub()
		if err != nil {
			return nil, err
		}
		lv := toFloat64OrZero(left)
		rv := toFloat64OrZero(right)
		switch op {
		case ">=":
			left = lv >= rv
		case "<=":
			left = lv <= rv
		case "==":
			left = lv == rv
		case "!=":
			left = lv != rv
		case ">":
			left = lv > rv
		case "<":
			left = lv < rv
		}
	}
	return left, nil
}

func (p *exprParser) parseAddSub() (interface{}, error) {
	left, err := p.parseMulDiv()
	if err != nil {
		return nil, err
	}
	for {
		p.skipWhitespace()
		if p.matchString("+") {
			right, err := p.parseMulDiv()
			if err != nil {
				return nil, err
			}
			left = toFloat64OrZero(left) + toFloat64OrZero(right)
		} else if p.matchString("-") {
			right, err := p.parseMulDiv()
			if err != nil {
				return nil, err
			}
			left = toFloat64OrZero(left) - toFloat64OrZero(right)
		} else {
			break
		}
	}
	return left, nil
}

func (p *exprParser) parseMulDiv() (interface{}, error) {
	left, err := p.parsePower()
	if err != nil {
		return nil, err
	}
	for {
		p.skipWhitespace()
		if p.matchString("*") {
			right, err := p.parsePower()
			if err != nil {
				return nil, err
			}
			left = toFloat64OrZero(left) * toFloat64OrZero(right)
		} else if p.matchString("/") {
			right, err := p.parsePower()
			if err != nil {
				return nil, err
			}
			rv := toFloat64OrZero(right)
			if rv == 0 {
				return nil, fmt.Errorf("division by zero")
			}
			left = toFloat64OrZero(left) / rv
		} else if p.matchString("%") {
			right, err := p.parsePower()
			if err != nil {
				return nil, err
			}
			lv := toFloat64OrZero(left)
			rv := toFloat64OrZero(right)
			if rv == 0 {
				return nil, fmt.Errorf("modulo by zero")
			}
			left = math.Mod(lv, rv)
		} else {
			break
		}
	}
	return left, nil
}

func (p *exprParser) parsePower() (interface{}, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	p.skipWhitespace()
	if p.matchString("**") {
		right, err := p.parsePower()
		if err != nil {
			return nil, err
		}
		lv := toFloat64OrZero(left)
		rv := toFloat64OrZero(right)
		if rv > 1000 {
			return nil, fmt.Errorf("exponent exceeds limit of 1000, got %v", rv)
		}
		left = math.Pow(lv, rv)
	}
	return left, nil
}

func (p *exprParser) parseUnary() (interface{}, error) {
	p.skipWhitespace()
	if p.matchString("-") {
		val, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return -toFloat64OrZero(val), nil
	}
	if p.matchString("+") {
		return p.parseUnary()
	}
	return p.parsePrimary()
}

func (p *exprParser) parsePrimary() (interface{}, error) {
	p.skipWhitespace()

	// Parenthesized expression
	if p.matchString("(") {
		val, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		p.skipWhitespace()
		if !p.matchString(")") {
			return nil, fmt.Errorf("expected ')' at position %d", p.pos)
		}
		return val, nil
	}

	// Number
	if num, ok := p.tryParseNumber(); ok {
		return num, nil
	}

	// Function call
	if name, ok := p.tryParseIdentifier(); ok {
		p.skipWhitespace()
		if p.matchString("(") {
			return p.parseFunctionCall(name)
		}
		// It's a variable name
		return name, nil
	}

	// IfExp (conditional: x if cond else y)
	// This is handled at a higher level

	return nil, fmt.Errorf("unexpected token at position %d", p.pos)
}

func (p *exprParser) parseFunctionCall(name string) (interface{}, error) {
	args := []interface{}{}
	p.skipWhitespace()
	if !p.matchString(")") {
		for {
			val, err := p.parseExpression()
			if err != nil {
				return nil, err
			}
			args = append(args, val)
			p.skipWhitespace()
			if p.matchString(",") {
				continue
			}
			break
		}
		p.skipWhitespace()
		if !p.matchString(")") {
			return nil, fmt.Errorf("expected ')' at position %d", p.pos)
		}
	}

	// Check custom functions first
	p.engine.mu.RLock()
	customFn, hasCustom := p.engine.customFunctions[name]
	p.engine.mu.RUnlock()
	if hasCustom {
		return customFn(args)
	}

	// Built-in functions
	return evalBuiltin(name, args)
}

func (p *exprParser) tryParseNumber() (interface{}, bool) {
	if p.pos >= len(p.input) {
		return nil, false
	}
	c := p.input[p.pos]
	if (c < '0' || c > '9') && c != '.' {
		return nil, false
	}
	start := p.pos
	hasDot := false
	for p.pos < len(p.input) {
		c := p.input[p.pos]
		if c >= '0' && c <= '9' {
			p.pos++
		} else if c == '.' && !hasDot {
			hasDot = true
			p.pos++
		} else {
			break
		}
	}
	if p.pos == start {
		return nil, false
	}
	numStr := p.input[start:p.pos]
	if hasDot {
		var f float64
		fmt.Sscanf(numStr, "%f", &f)
		return f, true
	}
	var i int
	fmt.Sscanf(numStr, "%d", &i)
	return i, true
}

func (p *exprParser) tryParseIdentifier() (string, bool) {
	if p.pos >= len(p.input) {
		return "", false
	}
	c := p.input[p.pos]
	if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_') {
		return "", false
	}
	start := p.pos
	for p.pos < len(p.input) {
		c := p.input[p.pos]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' {
			p.pos++
		} else {
			break
		}
	}
	return p.input[start:p.pos], true
}

func (p *exprParser) matchString(s string) bool {
	if p.pos+len(s) > len(p.input) {
		return false
	}
	if p.input[p.pos:p.pos+len(s)] != s {
		return false
	}
	p.pos += len(s)
	return true
}

func (p *exprParser) matchKeyword(kw string) bool {
	if p.pos+len(kw) > len(p.input) {
		return false
	}
	if p.input[p.pos:p.pos+len(kw)] != kw {
		return false
	}
	// Check word boundary
	endPos := p.pos + len(kw)
	if endPos < len(p.input) {
		c := p.input[endPos]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' {
			return false
		}
	}
	p.pos = endPos
	return true
}

func evalBuiltin(name string, args []interface{}) (interface{}, error) {
	switch name {
	case "abs":
		if len(args) != 1 {
			return nil, fmt.Errorf("abs expects 1 argument")
		}
		return math.Abs(toFloat64OrZero(args[0])), nil
	case "round":
		if len(args) < 1 || len(args) > 2 {
			return nil, fmt.Errorf("round expects 1-2 arguments")
		}
		v := toFloat64OrZero(args[0])
		precision := 0.0
		if len(args) == 2 {
			precision = toFloat64OrZero(args[1])
		}
		pow := math.Pow(10, precision)
		return math.Round(v*pow) / pow, nil
	case "min":
		if len(args) < 1 {
			return nil, fmt.Errorf("min expects at least 1 argument")
		}
		result := toFloat64OrZero(args[0])
		for _, a := range args[1:] {
			v := toFloat64OrZero(a)
			if v < result {
				result = v
			}
		}
		return result, nil
	case "max":
		if len(args) < 1 {
			return nil, fmt.Errorf("max expects at least 1 argument")
		}
		result := toFloat64OrZero(args[0])
		for _, a := range args[1:] {
			v := toFloat64OrZero(a)
			if v > result {
				result = v
			}
		}
		return result, nil
	case "pow":
		if len(args) != 2 {
			return nil, fmt.Errorf("pow expects 2 arguments")
		}
		base := toFloat64OrZero(args[0])
		exp := toFloat64OrZero(args[1])
		if exp > 1000 {
			return nil, fmt.Errorf("exponent exceeds limit of 1000")
		}
		return math.Pow(base, exp), nil
	case "sqrt":
		if len(args) != 1 {
			return nil, fmt.Errorf("sqrt expects 1 argument")
		}
		return math.Sqrt(toFloat64OrZero(args[0])), nil
	case "ceil":
		if len(args) != 1 {
			return nil, fmt.Errorf("ceil expects 1 argument")
		}
		return math.Ceil(toFloat64OrZero(args[0])), nil
	case "floor":
		if len(args) != 1 {
			return nil, fmt.Errorf("floor expects 1 argument")
		}
		return math.Floor(toFloat64OrZero(args[0])), nil
	case "log":
		if len(args) != 1 {
			return nil, fmt.Errorf("log expects 1 argument")
		}
		return math.Log(toFloat64OrZero(args[0])), nil
	case "log10":
		if len(args) != 1 {
			return nil, fmt.Errorf("log10 expects 1 argument")
		}
		return math.Log10(toFloat64OrZero(args[0])), nil
	case "int":
		if len(args) != 1 {
			return nil, fmt.Errorf("int expects 1 argument")
		}
		return int64(toFloat64OrZero(args[0])), nil
	case "float":
		if len(args) != 1 {
			return nil, fmt.Errorf("float expects 1 argument")
		}
		return toFloat64OrZero(args[0]), nil
	case "str":
		if len(args) != 1 {
			return nil, fmt.Errorf("str expects 1 argument")
		}
		return fmt.Sprintf("%v", args[0]), nil
	case "bool":
		if len(args) != 1 {
			return nil, fmt.Errorf("bool expects 1 argument")
		}
		return toBool(args[0]), nil
	default:
		return nil, fmt.Errorf("unknown function: %s", name)
	}
}

func toBool(v interface{}) bool {
	switch val := v.(type) {
	case bool:
		return val
	case int:
		return val != 0
	case int64:
		return val != 0
	case float64:
		return val != 0
	case string:
		return val != "" && val != "0" && val != "false"
	default:
		return v != nil
	}
}

func isDangerousName(name string) bool {
	dangerous := []string{
		"exec", "eval", "compile", "open", "input",
		"__import__", "globals", "locals", "vars", "dir",
		"getattr", "setattr", "delattr", "hasattr",
		"type", "object", "__builtins__", "__name__", "__file__",
		"__class__", "__bases__", "__subclasses__", "__mro__",
		"__self__", "__globals__", "__code__", "__func__",
		"__dict__", "__closure__",
		"os", "sys", "subprocess", "shutil", "pathlib",
		"socket", "ctypes", "signal", "io",
		"memoryview", "breakpoint", "help",
	}
	for _, d := range dangerous {
		if name == d {
			return true
		}
	}
	return false
}
