package api

// The preprocessing page and the expression workbench used to be pure echoes:
// PUT /preprocess/config wrote nothing and said updated:true, POST
// /expressions/evaluate answered result:null whatever you sent, and the rule and
// expression CRUD routes listed [] no matter what had been created. These tests
// hold the replacement to what the pages promise - a save that reaches the file
// and the store, a store that reaches the running engine, and a failure that
// says why.

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"edgelite/internal/config"
	"edgelite/internal/engine"
	"edgelite/internal/models"
	"edgelite/internal/storage"
)

// withPreprocessing installs a container carrying the rule store, the expression
// store and the two engines the collect path runs through, so a handler that
// claims it applied something can be checked against the engine.
func withPreprocessing(t *testing.T) *ServiceContainer {
	t.Helper()
	withIsolatedConfig(t)

	cfg := &config.AppConfig{}
	cfg.Database.SQLitePath = filepath.Join(t.TempDir(), "preprocess.db")
	db, err := storage.NewDatabase(cfg)
	if err != nil {
		t.Fatalf("test database: %v", err)
	}

	cont := GetContainer()
	cont.Database = db
	cont.PreprocessRules = storage.NewPreprocessRuleStore(db)
	cont.ExpressionConfigs = storage.NewExpressionConfigStore(db)
	cont.Preprocessor = engine.NewPreprocessor(&config.GetConfig().Preprocess)
	cont.ExpressionEngine = engine.NewPreprocessorExpressionEngine()
	t.Cleanup(func() { db.Close() })
	return cont
}

// pointConfigsOf decodes the data.point_configs object of a /preprocess/config
// response.
func pointConfigsOf(t *testing.T, body string) map[string]map[string]interface{} {
	t.Helper()
	var envelope struct {
		Code int `json:"code"`
		Data struct {
			Enabled      bool                              `json:"enabled"`
			PointConfigs map[string]map[string]interface{} `json:"point_configs"`
			DeviceRules  []map[string]interface{}          `json:"device_rules"`
		} `json:"data"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("unparseable response %s: %v", body, err)
	}
	return envelope.Data.PointConfigs
}

func TestPreprocessConfigSavePersistsAndReachesTheEngine(t *testing.T) {
	cont := withPreprocessing(t)

	c, rec := setupWithAdmin(echo.PUT, "/api/v1/preprocess/config",
		`{"global":{"enabled":true,"default_deadband":0,"default_filter_window":3,"default_aggregate_window_sec":0},`+
			`"points":{"boiler.temp":{"deadband":2,"deadband_percent":0}}}`)
	if err := handleUpdatePreprocessGlobalSettings(c); err != nil {
		t.Fatalf("PUT handler error: %v", err)
	}
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}

	if saved := persistedConfig(t); !saved.Preprocess.Enabled {
		t.Fatalf("the switch did not reach the config file: %+v", saved.Preprocess)
	}
	stored, err := cont.PreprocessRules.List()
	if err != nil {
		t.Fatalf("read back rules: %v", err)
	}
	if len(stored) != 1 || stored[0].PointName != "boiler.temp" || stored[0].DeviceID != "" {
		t.Fatalf("stored rules = %+v, want one gateway-wide rule for boiler.temp", stored)
	}

	// The store is only half the promise: the running preprocessor has to filter.
	if live := cont.Preprocessor.AllRules(); len(live) != 1 {
		t.Fatalf("the engine holds %d rules after a save, want 1", len(live))
	}
	if !cont.Preprocessor.Enabled() {
		t.Fatal("the engine still reports preprocessing off after the switch was saved on")
	}
	start := time.Now()
	out := cont.Preprocessor.Process("dev-any", []storage.PointData{
		{DeviceID: "dev-any", PointName: "boiler.temp", Value: 60.0, Timestamp: start},
		{DeviceID: "dev-any", PointName: "boiler.temp", Value: 61.0, Timestamp: start.Add(time.Second)},
		{DeviceID: "dev-any", PointName: "boiler.temp", Value: 70.0, Timestamp: start.Add(2 * time.Second)},
	})
	if len(out) != 2 {
		t.Fatalf("the saved deadband filtered nothing: %+v", out)
	}

	// What the page reads back has to be what is stored, or a refresh shows a
	// configuration the gateway is not running.
	c, rec = setupWithAdmin(echo.GET, "/api/v1/preprocess/config", "")
	if err := handleGetPreprocessGlobalSettings(c); err != nil {
		t.Fatalf("GET handler error: %v", err)
	}
	rows := pointConfigsOf(t, rec.Body.String())
	row, ok := rows["boiler.temp"]
	if !ok {
		t.Fatalf("GET did not list the saved row: %s", rec.Body)
	}
	if row["deadband"] != 2.0 {
		t.Fatalf("deadband read back as %v, want 2", row["deadband"])
	}
}

// TestPreprocessFilterParametersRoundTrip covers the filters whose tuning is not a
// sample window: the page now offers ema and kalman, so their parameters have to
// survive the store, reach the running engine, and come back in the GET view the
// table renders.
func TestPreprocessFilterParametersRoundTrip(t *testing.T) {
	cont := withPreprocessing(t)

	body := `{"global":{"enabled":true},"points":{` +
		`"meter.v":{"filter":"ema","ema_alpha":0.2,"filter_window":9},` +
		`"meter.w":{"filter":"kalman","kalman_process_noise":1000,"kalman_measurement_noise":0.001}}}`
	c, rec := setupWithAdmin(echo.PUT, "/api/v1/preprocess/config", body)
	if err := handleUpdatePreprocessGlobalSettings(c); err != nil {
		t.Fatalf("PUT handler error: %v", err)
	}
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}

	stored, err := cont.PreprocessRules.List()
	if err != nil {
		t.Fatalf("read rules: %v", err)
	}
	byPoint := map[string]models.PreprocessRule{}
	for _, rule := range stored {
		byPoint[rule.PointName] = rule
	}
	if len(stored) != 2 {
		t.Fatalf("stored rules = %+v, want one filter rule per row", stored)
	}
	ema := byPoint["meter.v"]
	if ema.Params["ema_alpha"] != 0.2 {
		t.Fatalf("the stored ema rule lost its alpha: %+v", ema.Params)
	}
	// ema reads no window, so storing the 9 the page still sends would advertise a
	// setting that changes nothing.
	if _, ok := ema.Params["filter_window"]; ok {
		t.Fatalf("the ema rule stored a filter window it never uses: %+v", ema.Params)
	}
	kalman := byPoint["meter.w"]
	if kalman.Params["kalman_process_noise"] != 1000.0 || kalman.Params["kalman_measurement_noise"] != 0.001 {
		t.Fatalf("the stored kalman rule lost its noise settings: %+v", kalman.Params)
	}

	// The engine has to run with those parameters, not with its own defaults.
	start := time.Now()
	step := func(name string) []storage.PointData {
		return []storage.PointData{
			{DeviceID: "dev-1", PointName: name, Value: 0, Timestamp: start},
			{DeviceID: "dev-1", PointName: name, Value: 0, Timestamp: start.Add(time.Second)},
			{DeviceID: "dev-1", PointName: name, Value: 100, Timestamp: start.Add(2 * time.Second)},
		}
	}
	smoothed := cont.Preprocessor.Process("dev-1", step("meter.v"))
	if got := valueOfFilter(t, smoothed[2].Value); got != 20.0 {
		t.Fatalf("ema at alpha 0.2 = %v, want 20 so the stored alpha is the one in force", got)
	}
	trusting := cont.Preprocessor.Process("dev-1", step("meter.w"))
	if got := valueOfFilter(t, trusting[2].Value); got < 90 {
		t.Fatalf("kalman with process noise 1000 = %v, want it to accept the step", got)
	}

	rows := pointConfigsOf(t, getConfigView(t))
	if rows["meter.v"]["ema_alpha"] != 0.2 {
		t.Fatalf("the page cannot read the alpha back: %+v", rows["meter.v"])
	}
	if _, ok := rows["meter.v"]["filter_window"]; ok {
		t.Fatalf("the page was shown a window the filter ignores: %+v", rows["meter.v"])
	}
	if rows["meter.w"]["kalman_process_noise"] != 1000.0 {
		t.Fatalf("the page cannot read the kalman settings back: %+v", rows["meter.w"])
	}
}

func valueOfFilter(t *testing.T, value interface{}) float64 {
	t.Helper()
	number, ok := value.(float64)
	if !ok {
		t.Fatalf("processed value is %T (%v), want a float64", value, value)
	}
	return number
}

func getConfigView(t *testing.T) string {
	t.Helper()
	c, rec := setupWithAdmin(echo.GET, "/api/v1/preprocess/config", "")
	if err := handleGetPreprocessGlobalSettings(c); err != nil {
		t.Fatalf("GET handler error: %v", err)
	}
	return rec.Body.String()
}

func TestPreprocessConfigSaveReplacesThePreviousRowSet(t *testing.T) {
	cont := withPreprocessing(t)

	c, _ := setupWithAdmin(echo.PUT, "/api/v1/preprocess/config",
		`{"global":{"enabled":true},"points":{"a.temp":{"deadband":1},"b.temp":{"deadband":2}}}`)
	if err := handleUpdatePreprocessGlobalSettings(c); err != nil {
		t.Fatalf("first PUT: %v", err)
	}

	// The page posts its whole table on every save, and a deleted row is simply
	// absent, so an absent row must go.
	c, rec := setupWithAdmin(echo.PUT, "/api/v1/preprocess/config",
		`{"global":{"enabled":true},"points":{"a.temp":{"deadband":1}}}`)
	if err := handleUpdatePreprocessGlobalSettings(c); err != nil {
		t.Fatalf("second PUT: %v", err)
	}
	if rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	stored, err := cont.PreprocessRules.List()
	if err != nil {
		t.Fatalf("read rules: %v", err)
	}
	if len(stored) != 1 || stored[0].PointName != "a.temp" {
		t.Fatalf("the removed row is still stored: %+v", stored)
	}
	if live := cont.Preprocessor.AllRules(); len(live) != 1 {
		t.Fatalf("the engine still runs %d rules, want 1", len(live))
	}
}

func TestPreprocessConfigSwitchOnlySaveKeepsTheRows(t *testing.T) {
	cont := withPreprocessing(t)

	c, _ := setupWithAdmin(echo.PUT, "/api/v1/preprocess/config",
		`{"global":{"enabled":true},"points":{"a.temp":{"filter":"median_5"}}}`)
	if err := handleUpdatePreprocessGlobalSettings(c); err != nil {
		t.Fatalf("PUT with a row: %v", err)
	}

	// A body that carries no points section at all must not clear the table; only
	// a posted set replaces it.
	c, rec := setupWithAdmin(echo.PUT, "/api/v1/preprocess/config", `{"global":{"enabled":false}}`)
	if err := handleUpdatePreprocessGlobalSettings(c); err != nil {
		t.Fatalf("PUT without points: %v", err)
	}
	if rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	stored, err := cont.PreprocessRules.List()
	if err != nil {
		t.Fatalf("read rules: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("a switch-only save dropped the rows: %+v", stored)
	}
	if saved := persistedConfig(t); saved.Preprocess.Enabled {
		t.Fatalf("the switch did not follow the body: %+v", saved.Preprocess)
	}
	if cont.Preprocessor.Enabled() {
		t.Fatal("the running engine still has preprocessing on after it was switched off")
	}
}

func TestPreprocessConfigRejectsWhatTheEngineCannotApply(t *testing.T) {
	cont := withPreprocessing(t)

	cases := []struct {
		name        string
		body        string
		wantMessage string
	}{
		{
			name:        "aggregation with no window filters nothing",
			body:        `{"global":{"enabled":true},"points":{"a.temp":{"aggregate":"avg"}}}`,
			wantMessage: "aggregate_window_sec",
		},
		{
			name:        "unknown filter type",
			body:        `{"global":{"enabled":true},"points":{"a.temp":{"filter":"butterworth"}}}`,
			wantMessage: "butterworth",
		},
		{
			name:        "unknown aggregate type",
			body:        `{"global":{"enabled":true},"points":{"a.temp":{"aggregate":"median","aggregate_window_sec":60}}}`,
			wantMessage: "median",
		},
		{
			name:        "a deadband of zero",
			body:        `{"global":{"enabled":true},"points":{"a.temp":{"deadband":0,"deadband_percent":0}}}`,
			wantMessage: "filters nothing",
		},
		{
			name:        "negative default window",
			body:        `{"global":{"enabled":true,"default_filter_window":0},"points":{}}`,
			wantMessage: "default_filter_window",
		},
		{
			name:        "a row that configures nothing",
			body:        `{"global":{"enabled":true},"points":{"a.temp":{}}}`,
			wantMessage: "nothing to store",
		},
		{
			name:        "alpha outside the range the engine accepts",
			body:        `{"global":{"enabled":true},"points":{"a.temp":{"filter":"ema","ema_alpha":3}}}`,
			wantMessage: "ema_alpha",
		},
		{
			name:        "alpha on a filter that does not read it",
			body:        `{"global":{"enabled":true},"points":{"a.temp":{"filter":"moving_avg","ema_alpha":0.5}}}`,
			wantMessage: "only applies to ema",
		},
		{
			name:        "kalman noise on a filter that does not read it",
			body:        `{"global":{"enabled":true},"points":{"a.temp":{"filter":"ema","kalman_process_noise":0.01}}}`,
			wantMessage: "apply to kalman",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, rec := setupWithAdmin(echo.PUT, "/api/v1/preprocess/config", tc.body)
			if err := handleUpdatePreprocessGlobalSettings(c); err != nil {
				t.Fatalf("handler error: %v", err)
			}
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
			}
			if !strings.Contains(rec.Body.String(), tc.wantMessage) {
				t.Fatalf("the rejection did not name the problem (%s): %s", tc.wantMessage, rec.Body)
			}
		})
	}

	stored, err := cont.PreprocessRules.List()
	if err != nil {
		t.Fatalf("read rules: %v", err)
	}
	if len(stored) != 0 {
		t.Fatalf("rejected rows were stored anyway: %+v", stored)
	}
	if saved := persistedConfig(t); saved.Preprocess.Enabled {
		t.Fatalf("a rejected save still switched preprocessing on: %+v", saved.Preprocess)
	}
}

func TestPreprocessRuleCRUDIsStoredAndApplied(t *testing.T) {
	cont := withPreprocessing(t)

	// The preprocess switch decides whether the engine filters at all, so a rule
	// created against a gateway with the switch off is stored without running.
	// Switch it on the way an operator would, then check the rule takes effect.
	c, rec := setupWithAdmin(echo.PUT, "/api/v1/preprocess/config", `{"global":{"enabled":true}}`)
	if err := handleUpdatePreprocessGlobalSettings(c); err != nil {
		t.Fatalf("switch PUT: %v", err)
	}
	if rec.Code != 200 {
		t.Fatalf("switch PUT status = %d: %s", rec.Code, rec.Body)
	}

	// A rule with no device would live in the bucket the page replaces wholesale,
	// so it has to be refused rather than quietly deleted by the next page save.
	c, rec = setupWithAdmin(echo.POST, "/api/v1/preprocess/rules",
		`{"point_name":"temp","operation":"clamp","params":{"min":0,"max":100},"enabled":true}`)
	if err := handleCreatePreprocessRule(c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a device-less rule got %d, want 400: %s", rec.Code, rec.Body)
	}

	c, rec = setupWithAdmin(echo.POST, "/api/v1/preprocess/rules",
		`{"device_id":"dev-1","point_name":"temp","operation":"transform","params":{"scale":1.8,"offset":32},"enabled":true}`)
	if err := handleCreatePreprocessRule(c); err != nil {
		t.Fatalf("create handler error: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body)
	}
	var created struct {
		Data struct {
			ID       string `json:"id"`
			DeviceID string `json:"device_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("created response: %v", err)
	}
	if !strings.HasPrefix(created.Data.ID, "pp-") {
		t.Fatalf("created id = %q, want a generated one", created.Data.ID)
	}

	start := time.Now()
	out := cont.Preprocessor.Process("dev-1", []storage.PointData{
		{DeviceID: "dev-1", PointName: "temp", Value: 100.0, Timestamp: start},
	})
	if len(out) != 1 || out[0].Value != 212.0 {
		t.Fatalf("the created rule is not running: %+v", out)
	}

	// An update that carries only the params must keep the rest of the record.
	c, rec = setupWithAdmin(echo.PUT, "/api/v1/preprocess/rules/"+created.Data.ID,
		`{"params":{"scale":2,"offset":0}}`)
	c.SetParamNames("id")
	c.SetParamValues(created.Data.ID)
	if err := handleUpdatePreprocessRule(c); err != nil {
		t.Fatalf("update handler error: %v", err)
	}
	if rec.Code != 200 {
		t.Fatalf("update status = %d: %s", rec.Code, rec.Body)
	}
	out = cont.Preprocessor.Process("dev-1", []storage.PointData{
		{DeviceID: "dev-1", PointName: "temp", Value: 100.0, Timestamp: start},
	})
	if len(out) != 1 || out[0].Value != 200.0 {
		t.Fatalf("the edited rule is not running: %+v", out)
	}

	// A rule that does nothing must not be creatable in the first place.
	c, rec = setupWithAdmin(echo.POST, "/api/v1/preprocess/rules",
		`{"device_id":"dev-1","point_name":"temp","operation":"clamp","params":{"min":10,"max":5},"enabled":true}`)
	if err := handleCreatePreprocessRule(c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("an inverted clamp got %d, want 400: %s", rec.Code, rec.Body)
	}

	c, rec = setupWithAdmin(echo.DELETE, "/api/v1/preprocess/rules/"+created.Data.ID, "")
	c.SetParamNames("id")
	c.SetParamValues(created.Data.ID)
	if err := handleDeletePreprocessRule(c); err != nil {
		t.Fatalf("delete handler error: %v", err)
	}
	if rec.Code != 200 {
		t.Fatalf("delete status = %d: %s", rec.Code, rec.Body)
	}
	if live := cont.Preprocessor.AllRules(); len(live) != 0 {
		t.Fatalf("the deleted rule is still running: %+v", live)
	}

	c, rec = setupWithAdmin(echo.DELETE, "/api/v1/preprocess/rules/"+created.Data.ID, "")
	c.SetParamNames("id")
	c.SetParamValues(created.Data.ID)
	if err := handleDeletePreprocessRule(c); err != nil {
		t.Fatalf("second delete handler error: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("deleting a rule that is not there got %d, want 404: %s", rec.Code, rec.Body)
	}
}

func TestDerivedExpressionCRUDAndTestEndpoint(t *testing.T) {
	cont := withPreprocessing(t)

	c, rec := setupWithAdmin(echo.POST, "/api/v1/expressions/configs",
		`{"name":"bad","expression":"sin(${a})","output_point":"derived.sin","enabled":true}`)
	if err := handleCreateExpressionConfig(c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("an expression this parser cannot compute got %d, want 400: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "unknown function") {
		t.Fatalf("the rejection did not say what is wrong: %s", rec.Body)
	}

	c, rec = setupWithAdmin(echo.POST, "/api/v1/expressions/configs",
		`{"name":"mean of two","expression":"(${a} + ${b}) / 2","output_point":"derived.mean","enabled":false}`)
	if err := handleCreateExpressionConfig(c); err != nil {
		t.Fatalf("create handler error: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body)
	}
	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("created response: %v", err)
	}
	if len(cont.ExpressionEngine.GetExpressions()) != 1 {
		t.Fatalf("the created expression never reached the engine")
	}

	// An off expression must not produce a point on a live batch.
	derived := cont.ExpressionEngine.Evaluate("dev-1", map[string]interface{}{"a": 2.0, "b": 4.0})
	if len(derived) != 0 {
		t.Fatalf("a disabled expression produced points: %+v", derived)
	}

	c, rec = setupWithAdmin(echo.PUT, "/api/v1/expressions/configs/"+created.Data.ID, `{"enabled":true}`)
	c.SetParamNames("id")
	c.SetParamValues(created.Data.ID)
	if err := handleUpdateExpressionConfig(c); err != nil {
		t.Fatalf("update handler error: %v", err)
	}
	if rec.Code != 200 {
		t.Fatalf("update status = %d: %s", rec.Code, rec.Body)
	}
	derived = cont.ExpressionEngine.Evaluate("dev-1", map[string]interface{}{"a": 2.0, "b": 4.0})
	if len(derived) != 1 || derived[0].Value != 3.0 || derived[0].PointName != "derived.mean" {
		t.Fatalf("the enabled expression does not derive the point: %+v", derived)
	}

	c, rec = setupWithAdmin(echo.POST, "/api/v1/expressions/test",
		`{"expression":"${a} * 1.8 + 32","context":{"a":100}}`)
	if err := handleTestExpression(c); err != nil {
		t.Fatalf("test handler error: %v", err)
	}
	if !strings.Contains(rec.Body.String(), "212") {
		t.Fatalf("the test endpoint did not compute the expression: %s", rec.Body)
	}

	c, rec = setupWithAdmin(echo.POST, "/api/v1/expressions/test", `{"expression":"${missing} + 1"}`)
	if err := handleTestExpression(c); err != nil {
		t.Fatalf("test handler error: %v", err)
	}
	if !strings.Contains(rec.Body.String(), "undefined variables") {
		t.Fatalf("a failed test was reported as a success: %s", rec.Body)
	}
}

func TestExpressionWorkbenchComputesAndReports(t *testing.T) {
	withPreprocessing(t)

	c, rec := setupWithAdmin(echo.POST, "/api/v1/expressions/evaluate",
		`{"expression":"${sensor.temp} * 2","variables":{"sensor.temp":21.5}}`)
	if err := handleEvaluateExpressionReal(c); err != nil {
		t.Fatalf("evaluate handler error: %v", err)
	}
	var evaluated struct {
		Data struct {
			Result float64 `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &evaluated); err != nil {
		t.Fatalf("evaluate response %s: %v", rec.Body, err)
	}
	if evaluated.Data.Result != 43 {
		t.Fatalf("result = %v, want 43", evaluated.Data.Result)
	}

	// The failure has to reach the operator with the parser's reason, which is
	// what the old result:null answer hid.
	c, rec = setupWithAdmin(echo.POST, "/api/v1/expressions/evaluate", `{"expression":"${nope} * 2"}`)
	if err := handleEvaluateExpressionReal(c); err != nil {
		t.Fatalf("evaluate handler error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("an unresolvable expression got %d, want 400: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "undefined variables: nope") {
		t.Fatalf("the response did not name the missing variable: %s", rec.Body)
	}

	c, rec = setupWithAdmin(echo.POST, "/api/v1/expressions/evaluate-batch",
		`{"expressions":{"fahrenheit":"${sensor.temp} * 1.8 + 32","broken":"nope("},"variables":{"sensor.temp":100}}`)
	if err := handleEvaluateBatchExpressionReal(c); err != nil {
		t.Fatalf("batch handler error: %v", err)
	}
	var batch struct {
		Data struct {
			Results map[string]interface{} `json:"results"`
			Errors  map[string]string      `json:"errors"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &batch); err != nil {
		t.Fatalf("batch response %s: %v", rec.Body, err)
	}
	if batch.Data.Results["fahrenheit"] != 212.0 {
		t.Fatalf("batch results = %+v, want fahrenheit 212", batch.Data.Results)
	}
	if _, ok := batch.Data.Errors["broken"]; !ok {
		t.Fatalf("the failed entry was not reported: %s", rec.Body)
	}

	c, rec = setupWithAdmin(echo.POST, "/api/v1/expressions/validate",
		`{"expression":"${a} > 10 and ${b} < 5","variables":{"a":1,"b":2}}`)
	if err := handleValidateExpressionReal(c); err != nil {
		t.Fatalf("validate handler error: %v", err)
	}
	if !strings.Contains(rec.Body.String(), `"valid":true`) {
		t.Fatalf("a computable expression was not validated: %s", rec.Body)
	}
}

func TestExpressionCatalogOnlyListsWhatThisParserComputes(t *testing.T) {
	c, rec := setupWithAdmin(echo.GET, "/api/v1/expressions/functions", "")
	if err := handleGetExpressionFunctionsReal(c); err != nil {
		t.Fatalf("functions handler error: %v", err)
	}
	var payload struct {
		Data struct {
			Functions []struct {
				Name        string `json:"name"`
				Description string `json:"description"`
				Example     string `json:"example"`
			} `json:"functions"`
			Operators []struct {
				Symbol      string `json:"symbol"`
				Description string `json:"description"`
			} `json:"operators"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("functions response %s: %v", rec.Body, err)
	}
	if len(payload.Data.Functions) == 0 || len(payload.Data.Operators) == 0 {
		t.Fatalf("the reference tables are empty: %s", rec.Body)
	}

	variables := map[string]interface{}{"demo.value": 4.5}
	for _, fn := range payload.Data.Functions {
		if _, err := expressionEvaluator.EvaluateDetailed(fn.Example, variables); err != nil {
			t.Errorf("the page advertises %s as %q but the parser refuses it (%s): %v",
				fn.Name, fn.Description, fn.Example, err)
		}
	}

	operatorSamples := map[string]string{
		"+":   "1 + 2",
		"-":   "5 - 2",
		"*":   "3 * 4",
		"/":   "8 / 2",
		"%":   "9 % 4",
		"**":  "2 ** 3",
		"==":  "2 == 2",
		"!=":  "2 != 3",
		"<":   "2 < 3",
		"<=":  "3 <= 3",
		">":   "4 > 3",
		">=":  "4 >= 4",
		"and": "1 and 1",
		"or":  "0 or 1",
		"not": "not 0",
	}
	for _, op := range payload.Data.Operators {
		sample, ok := operatorSamples[op.Symbol]
		if !ok {
			t.Errorf("the page advertises operator %q, which this test does not know", op.Symbol)
			continue
		}
		if _, err := expressionEvaluator.EvaluateDetailed(sample, nil); err != nil {
			t.Errorf("the page advertises %q as %q but the parser refuses %q: %v", op.Symbol, op.Description, sample, err)
		}
	}
}
