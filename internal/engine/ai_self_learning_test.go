package engine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"edgelite/internal/config"
)

// The self-learning family answered every failure as a result: a sidecar that
// refused (HTTP 200 + success:false, or HTTP 400) was read as "no anomaly", and
// the local model's absence was reported as predicted_value 0. These tests pin the
// three-way distinction the gateway now has to keep: the sidecar answered, the
// sidecar refused, and nothing is there at all. Each call is routed to the learner
// that holds the key, and every answer says which learner produced it.

type slReply struct {
	status int
	body   map[string]interface{}
}

// newSelfLearningSidecar stubs the sidecar's self-learning routes. A path with no
// entry answers 500 with an HTML body, which is what a proxy in front of a dead
// sidecar returns - deliberately not a parseable envelope.
func newSelfLearningSidecar(t *testing.T, replies map[string]slReply) (*httptest.Server, *[]sidecarCall) {
	t.Helper()
	var calls []sidecarCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dec := json.NewDecoder(r.Body)
		var body map[string]interface{}
		_ = dec.Decode(&body)
		if body == nil {
			body = map[string]interface{}{}
		}
		calls = append(calls, sidecarCall{Path: r.URL.Path, Body: body})
		reply, ok := replies[r.URL.Path]
		if !ok {
			w.Header().Set("Content-Type", "text/html")
			http.Error(w, "no route "+r.URL.Path, http.StatusInternalServerError)
			return
		}
		status := reply.status
		if status == 0 {
			status = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(reply.body)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func newLocalOnlyEngine() *AIInferenceEngine {
	return NewAIInferenceEngine(&config.AiInferenceConfig{
		Enabled:                 true,
		ModelsDir:               "models",
		SidecarURL:              "http://127.0.0.1:1",
		MaxConcurrentInferences: 1,
	}, nil)
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestSelfLearningLocalOnlyTrainAndPredict covers the gateway-alone path: no
// sidecar is listening, so the in-process EWMA learner has to do the work and say
// that it did.
func TestSelfLearningLocalOnlyTrainAndPredict(t *testing.T) {
	eng := newLocalOnlyEngine()
	ctx := testCtx(t)

	for i := 0; i < 12; i++ {
		isAnomaly, _, source, err := eng.AddSample(ctx, "dev-1", "temp", 50.0, 100)
		if err != nil {
			t.Fatalf("sample %d: %v", i, err)
		}
		if source != SelfLearningSourceLocal {
			t.Fatalf("sample %d came from %q, want local with no sidecar listening", i, source)
		}
		if isAnomaly {
			t.Fatalf("sample %d of a flat signal flagged as an anomaly", i)
		}
	}
	isAnomaly, confidence, source, err := eng.AddSample(ctx, "dev-1", "temp", 5000.0, 100)
	if err != nil {
		t.Fatalf("spike: %v", err)
	}
	if !isAnomaly {
		t.Fatal("a 5000.0 spike on a flat 50.0 window was not flagged")
	}
	if source != SelfLearningSourceLocal || confidence <= 0 {
		t.Fatalf("spike verdict = source %q confidence %v, want local with a usable confidence", source, confidence)
	}

	predicted, predictConf, source, err := eng.Predict(ctx, "dev-1", "temp")
	if err != nil {
		t.Fatalf("predict: %v", err)
	}
	if source != SelfLearningSourceLocal {
		t.Fatalf("predict source = %q, want local", source)
	}
	if predicted <= 50.0 || predicted >= 5000.0 {
		t.Fatalf("predicted %v is not between the trained value and the spike", predicted)
	}
	if predictConf != confidence {
		t.Fatalf("predict confidence %v disagrees with the sample verdict %v", predictConf, confidence)
	}

	stats, source, err := eng.GetSelfLearningStats(ctx, "dev-1", "temp")
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if source != SelfLearningSourceLocal {
		t.Fatalf("stats source = %q, want local", source)
	}
	if got, _ := stats["total_samples"].(int); got != 13 {
		t.Fatalf("total_samples = %#v, want the 13 samples that were recorded", stats["total_samples"])
	}
	// One model, one confidence: GetStats and Predict used to derive it with two
	// different formulas, so the same model reported 1.0 to its own page and 0.0
	// to the caller asking for a prediction.
	if got, _ := stats["confidence"].(float64); got != confidence {
		t.Fatalf("stats confidence %#v disagrees with predict confidence %v", stats["confidence"], confidence)
	}

	rows, source, err := eng.GetAllSelfLearningStats(ctx)
	if err != nil {
		t.Fatalf("stats/all: %v", err)
	}
	if source != SelfLearningSourceLocal || len(rows) != 1 {
		t.Fatalf("stats/all = %d rows from %q, want 1 local row", len(rows), source)
	}
	if rows[0]["source"] != SelfLearningSourceLocal {
		t.Fatalf("row is not stamped with its learner: %#v", rows[0])
	}

	// Reset reports what it wiped, and 404s when there is nothing to wipe: the
	// handler used to answer success:true either way.
	if _, err := eng.ResetSelfLearning(ctx, "dev-never-sampled", "temp"); !errors.Is(err, ErrSelfLearningNotFound) {
		t.Fatalf("reset of an unknown key = %v, want ErrSelfLearningNotFound", err)
	}
	if source, err := eng.ResetSelfLearning(ctx, "dev-1", "temp"); err != nil || source != SelfLearningSourceLocal {
		t.Fatalf("reset of the trained model = (%q, %v), want local and no error", source, err)
	}
	stats, _, err = eng.GetSelfLearningStats(ctx, "dev-1", "temp")
	if err != nil {
		t.Fatalf("stats after reset: %v", err)
	}
	if got, _ := stats["total_samples"].(int); got != 0 {
		t.Fatalf("total_samples = %#v after a reset, want 0", stats["total_samples"])
	}
	if got, _ := stats["last_anomaly"].(string); got != "" {
		t.Fatalf("last_anomaly = %#v after a reset, want the sidecar's empty-string convention", stats["last_anomaly"])
	}
}

// TestSelfLearningRejectsAreNotAnsweredLocally is the divergence fix: for a key
// the sidecar owns, its refusals are its answer. Falling back to gateway state on
// any error let the gateway keep learning in a copy nobody would ever read.
func TestSelfLearningRejectsAreNotAnsweredLocally(t *testing.T) {
	rejection := map[string]slReply{
		"/self-learning/stats": {body: map[string]interface{}{"stats": map[string]interface{}{
			"device_id": "dev-1", "point_name": "temp", "total_samples": 99,
			"threshold": 2.5,
		}}},
		"/self-learning/sample": {body: map[string]interface{}{
			"success":       false,
			"error_code":    "ERR_INVALID_REQUEST",
			"error_message": "value must be a number",
		}},
		"/self-learning/reset": {body: map[string]interface{}{
			"success":    false,
			"error_code": errSelfLearningNotFoundCode,
		}},
		"/self-learning/threshold": {status: http.StatusBadRequest, body: map[string]interface{}{
			"success":       false,
			"error_code":    "ERR_INVALID_REQUEST",
			"error_message": "threshold must be positive",
		}},
		"/self-learning/predict": {status: http.StatusBadGateway, body: map[string]interface{}{
			"success":       false,
			"error_code":    "ERR_SIDECAR_INTERNAL",
			"error_message": "model registry is corrupted",
		}},
	}
	srv, _ := newSelfLearningSidecar(t, rejection)
	eng := newSidecarEngine(srv.URL)
	ctx := testCtx(t)
	// Seed the local registry directly: if any of the calls below falls back, the
	// rows it learns would prove it.
	eng.selfLearner.GetOrCreate("dev-1", "temp", 100).AddSample(50.0)

	if _, _, _, err := eng.AddSample(ctx, "dev-1", "temp", 42.0, 100); err == nil {
		t.Fatal("a success:false sample was reported as a verdict")
	} else if errors.Is(err, ErrSidecarUnreachable) {
		t.Fatalf("a refusal was classified as unreachable: %v", err)
	}

	stats, source, err := eng.GetSelfLearningStats(ctx, "dev-1", "temp")
	if err != nil {
		t.Fatalf("stats of a key the sidecar owns: %v", err)
	}
	if source != SelfLearningSourceSidecar {
		t.Fatalf("stats source = %q, want the sidecar's own answer", source)
	}
	if got, _ := stats["total_samples"].(int64); got != 99 {
		t.Fatalf("stats = %#v, want the sidecar's 99 samples, not the local model's 1", stats["total_samples"])
	}
	// The threshold is the one value the operator writes by hand, so the engine
	// has to carry it out of the sidecar's answer instead of dropping it.
	if got, _ := stats["threshold"].(float64); got != 2.5 {
		t.Fatalf("threshold = %#v, want the sidecar's 2.5", stats["threshold"])
	}
	if _, _, _, err := eng.Predict(ctx, "dev-1", "temp"); err == nil {
		t.Fatal("an HTTP 502 predict was reported as a prediction")
	} else if errors.Is(err, ErrSidecarUnreachable) {
		t.Fatalf("an HTTP 502 was classified as unreachable: %v", err)
	}
	if _, err := eng.ResetSelfLearning(ctx, "dev-1", "temp"); !errors.Is(err, ErrSelfLearningNotFound) {
		t.Fatalf("reset refused by the owning sidecar = %v, want ErrSelfLearningNotFound", err)
	}
	if _, err := eng.SetSelfLearningThreshold(ctx, "dev-1", "temp", 2.0); err == nil {
		t.Fatal("an HTTP 400 threshold write was reported as applied")
	} else if errors.Is(err, ErrSidecarUnreachable) {
		t.Fatalf("an HTTP 400 was classified as unreachable: %v", err)
	}
	// And the refusals left the gateway's own copy untouched.
	trained := eng.selfLearner.GetModel("dev-1", "temp").GetStats()
	if got, _ := trained["total_samples"].(int); got != 1 {
		t.Fatalf("local model holds %d samples, want the 1 it was seeded with", got)
	}
}

// TestSelfLearningWritesReachTheLearnerThatHoldsTheKey is the other half of the
// same rule. A key the sidecar disowns but the gateway trained while it was away
// is listed as a local row, so its four buttons have to act on that row: sending
// the call to the sidecar instead either created a second empty model there (and
// reported "applied" for a row that did not change) or 404d on a row the operator
// can see. The proof is the request log: nothing is asked of the sidecar but the
// ownership probe.
func TestSelfLearningWritesReachTheLearnerThatHoldsTheKey(t *testing.T) {
	srv, calls := newSelfLearningSidecar(t, map[string]slReply{
		"/self-learning/stats": {body: map[string]interface{}{"stats": nil}},
		"/self-learning/threshold": {body: map[string]interface{}{
			"success": false, "error_code": "ERR_WOULD_CREATE_A_SECOND_MODEL",
		}},
		// What a real sidecar says for a key it has no model for - the answer that
		// makes "reset something that does not exist" a 404 rather than a success.
		"/self-learning/reset": {body: map[string]interface{}{
			"success": false, "error_code": errSelfLearningNotFoundCode,
		}},
	})
	eng := newSidecarEngine(srv.URL)
	ctx := testCtx(t)
	eng.selfLearner.GetOrCreate("dev-1", "temp", 100).AddSample(50.0)

	if _, _, source, err := eng.AddSample(ctx, "dev-1", "temp", 60.0, 100); err != nil || source != SelfLearningSourceLocal {
		t.Fatalf("sample = (%q, %v), want the local model to take it", source, err)
	}
	if _, _, source, err := eng.Predict(ctx, "dev-1", "temp"); err != nil || source != SelfLearningSourceLocal {
		t.Fatalf("predict = (%q, %v), want the local model's answer", source, err)
	}
	if source, err := eng.SetSelfLearningThreshold(ctx, "dev-1", "temp", 1.5); err != nil || source != SelfLearningSourceLocal {
		t.Fatalf("threshold = (%q, %v), want it applied to the local model", source, err)
	}
	if (*calls)[len(*calls)-1].Path != "/self-learning/stats" {
		t.Fatalf("the sidecar was asked to write a key it disowns: last call %s", (*calls)[len(*calls)-1].Path)
	}
	if got := eng.selfLearner.GetModel("dev-1", "temp").GetStats(); got["total_samples"] != 2 {
		t.Fatalf("local model has %#v samples, want the seeded one plus the recorded one", got["total_samples"])
	}
	if source, err := eng.ResetSelfLearning(ctx, "dev-1", "temp"); err != nil || source != SelfLearningSourceLocal {
		t.Fatalf("reset = (%q, %v), want the local model wiped", source, err)
	}
	if got := eng.selfLearner.GetModel("dev-1", "temp").GetStats(); got["total_samples"] != 0 {
		t.Fatalf("local model still holds %#v samples after its reset", got["total_samples"])
	}
	// A key neither learner holds stays neither learner's: the reset is a 404, not
	// a success that wiped nothing.
	if _, err := eng.ResetSelfLearning(ctx, "dev-nothing", "temp"); !errors.Is(err, ErrSelfLearningNotFound) {
		t.Fatalf("reset of a key nobody holds = %v, want ErrSelfLearningNotFound", err)
	}
}

// TestSelfLearningStatsOfAnEmptySidecarIsNotLocalState is the other half of the
// same rule: the listing is the sidecar's registry first, and models the gateway
// trained while nothing answered are appended under their own stamp - so turning
// the sidecar on neither hides trained state nor presents local state as its data.
func TestSelfLearningStatsOfAnEmptySidecarIsNotLocalState(t *testing.T) {
	srv, calls := newSelfLearningSidecar(t, map[string]slReply{
		"/self-learning/stats/all": {body: map[string]interface{}{"stats": []interface{}{}}},
		"/self-learning/predict":   {body: map[string]interface{}{"predicted_value": 7.5, "confidence": 0.25}},
	})
	eng := newSidecarEngine(srv.URL)
	eng.selfLearner.GetOrCreate("dev-1", "temp", 100).AddSample(50.0)
	ctx := testCtx(t)

	rows, source, err := eng.GetAllSelfLearningStats(ctx)
	if err != nil {
		t.Fatalf("stats/all: %v", err)
	}
	if len(*calls) == 0 {
		t.Fatal("the listing answered without asking the sidecar")
	}
	if len(rows) != 1 || source != SelfLearningSourceLocal {
		t.Fatalf("stats/all = %d rows from %q, want the one local model over an empty sidecar registry", len(rows), source)
	}
	if rows[0]["source"] != SelfLearningSourceLocal {
		t.Fatalf("a gateway row was listed without its stamp: %#v", rows[0])
	}

	// Once the sidecar reports a key itself, its row wins and the gateway's copy of
	// that key is not listed again beside it.
	srv2, _ := newSelfLearningSidecar(t, map[string]slReply{
		"/self-learning/stats/all": {body: map[string]interface{}{"stats": []interface{}{
			map[string]interface{}{"device_id": "dev-1", "point_name": "temp", "total_samples": 99},
		}}},
	})
	eng2 := newSidecarEngine(srv2.URL)
	eng2.selfLearner.GetOrCreate("dev-1", "temp", 100).AddSample(50.0)
	eng2.selfLearner.GetOrCreate("dev-2", "press", 100).AddSample(50.0)

	rows2, source2, err := eng2.GetAllSelfLearningStats(ctx)
	if err != nil {
		t.Fatalf("stats/all with a live registry: %v", err)
	}
	if source2 != SelfLearningSourceMixed || len(rows2) != 2 {
		t.Fatalf("stats/all = %d rows from %q, want the sidecar row plus one local row", len(rows2), source2)
	}
	if rows2[0]["source"] != SelfLearningSourceSidecar || rows2[0]["total_samples"] != int64(99) {
		t.Fatalf("the sidecar's row is not first or not its own: %#v", rows2[0])
	}
	// This stub answers without a threshold, which is what a sidecar predating the
	// field does: nil renders as unknown, where a 0 would claim a threshold of 0.
	if rows2[0]["threshold"] != nil {
		t.Fatalf("an unreported threshold reached the ledger as %#v", rows2[0]["threshold"])
	}
	if rows2[1]["source"] != SelfLearningSourceLocal || rows2[1]["device_id"] != "dev-2" {
		t.Fatalf("only the extra key may be appended, and stamped local: %#v", rows2[1])
	}

	predicted, confidence, source, err := eng.Predict(ctx, "dev-1", "temp")
	if err != nil {
		t.Fatalf("predict: %v", err)
	}
	if source != SelfLearningSourceSidecar || predicted != 7.5 || confidence != 0.25 {
		t.Fatalf("predict = %v/%v from %q, want the sidecar's 7.5/0.25", predicted, confidence, source)
	}

	// A route the sidecar does not serve answers 500 + HTML. That is still an
	// answer: the gateway must not train its own model behind a live sidecar.
	if _, _, _, err := eng.AddSample(ctx, "dev-1", "temp", 51.0, 100); err == nil {
		t.Fatal("a 500 from the sidecar was reported as a verdict")
	}
	if got := eng.selfLearner.GetModel("dev-1", "temp").GetStats(); got["total_samples"] != 1 {
		t.Fatalf("the gateway trained behind a live sidecar: %#v", got)
	}
}

// TestSelfLearningUnreachableIsClassifiedAsSuch guards the classification itself:
// only a request that got no response at all licenses gateway-side learning.
func TestSelfLearningUnreachableIsClassifiedAsSuch(t *testing.T) {
	_, err := newLocalOnlyEngine().client.GetAllSelfLearningStats(testCtx(t))
	if !errors.Is(err, ErrSidecarUnreachable) {
		t.Fatalf("connection refused = %v, want ErrSidecarUnreachable", err)
	}
	if errors.Is(err, ErrSidecarRejected) {
		t.Fatalf("connection refused = %v, must not also be a rejection", err)
	}

	// A hang is an unreachable sidecar too, and the caller's deadline is what
	// bounds it now that the handlers pass their own context down.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(500 * time.Millisecond):
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"stats": []interface{}{}})
		}
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(testCtx(t), 50*time.Millisecond)
	defer cancel()
	if _, err := newSidecarEngine(srv.URL).client.GetAllSelfLearningStats(ctx); !errors.Is(err, ErrSidecarUnreachable) {
		t.Fatalf("a deadline while waiting = %v, want ErrSidecarUnreachable", err)
	}
}

// TestSelfLearningThresholdFollowsTheSidecarSemantics: a threshold write is a
// get-or-create in both learners, and the value the caller asked for has to reach
// the model instead of the default.
func TestSelfLearningThresholdFollowsTheSidecarSemantics(t *testing.T) {
	eng := newLocalOnlyEngine()
	ctx := testCtx(t)
	source, err := eng.SetSelfLearningThreshold(ctx, "dev-2", "pressure", 0.5)
	if err != nil {
		t.Fatalf("threshold: %v", err)
	}
	if source != SelfLearningSourceLocal {
		t.Fatalf("threshold source = %q, want local", source)
	}
	model := eng.selfLearner.GetModel("dev-2", "pressure")
	if model == nil {
		t.Fatal("threshold set did not create the model the sidecar would create")
	}
	model.mu.Lock()
	got := model.threshold
	model.mu.Unlock()
	if got != 0.5 {
		t.Fatalf("threshold = %v, want the 0.5 the caller asked for", got)
	}

	// An unbounded window is a memory lease the gateway cannot take back.
	if _, _, _, err := eng.AddSample(ctx, "dev-3", "big", 1.0, 1<<30); err != nil {
		t.Fatalf("oversized window: %v", err)
	}
	big := eng.selfLearner.GetModel("dev-3", "big")
	big.mu.Lock()
	clamped := big.windowSize
	big.mu.Unlock()
	if clamped != MaxSelfLearningWindow {
		t.Fatalf("windowSize = %d, want it clamped to %d", clamped, MaxSelfLearningWindow)
	}
}

// TestSelfLearningEWMAIsNotSeededOnZero: the first sample initialises the average,
// so a signal that sits at 0 and then jumps is smoothed from its real history.
// Before this, a running average that happened to be exactly 0 restarted at the
// next value, which erased the window.
func TestSelfLearningEWMAIsNotSeededOnZero(t *testing.T) {
	model := NewSelfLearningModel("dev-4", "zero", 100)
	for i := 0; i < 5; i++ {
		model.AddSample(0.0)
	}
	model.AddSample(10.0)
	if got := model.Predict(); got != 3.0 {
		t.Fatalf("ewma after 5 zeros then 10 = %v, want 0.3*10 = 3", got)
	}
	model.Reset()
	if model.Predict() != 0 {
		t.Fatal("reset left the average behind")
	}
	model.AddSample(4.0)
	if got := model.Predict(); got != 4.0 {
		t.Fatalf("first sample after a reset = %v, want it to seed the average", got)
	}
}
