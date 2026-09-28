package api

import (
	"net/http"
	"testing"

	"github.com/labstack/echo/v4"
)

// The per-model preprocess/postprocess surface used to split its own story: the
// writes answered 501 "not supported" while the reads answered 200 with
// `steps: []` and a made-up step catalogue. A client that read first saw a
// configured-but-empty pipeline, which is a different claim from "no pipeline
// can exist here". These tests pin read and write to the same verdict.

func TestAIPipelineReadsAnswerTheSameAsWrites(t *testing.T) {
	cases := []struct {
		name        string
		read        func(echo.Context) error
		write       func(echo.Context) error
		wantCode    int
		wantErrCode string
	}{
		{
			name:        "preprocess",
			read:        handleGetPreprocessConfig,
			write:       handleSetPreprocessConfig,
			wantCode:    http.StatusNotImplemented,
			wantErrCode: "ERR_AI_PREPROCESS_UNSUPPORTED",
		},
		{
			name:        "postprocess",
			read:        handleGetPostprocessConfig,
			write:       handleSetPostprocessConfig,
			wantCode:    http.StatusNotImplemented,
			wantErrCode: "ERR_AI_POSTPROCESS_UNSUPPORTED",
		},
	}
	for _, tc := range cases {
		for kind, h := range map[string]func(echo.Context) error{"read": tc.read, "write": tc.write} {
			code, ec, data := callAI(t, h, http.MethodGet, "/api/v1/ai/models/m1/"+tc.name, `{"steps":["normalize"]}`)
			if code != tc.wantCode {
				t.Fatalf("%s %s: status = %d, want %d", tc.name, kind, code, tc.wantCode)
			}
			if ec != tc.wantErrCode {
				t.Fatalf("%s %s: error_code = %q, want %q", tc.name, kind, ec, tc.wantErrCode)
			}
			if data != nil {
				t.Fatalf("%s %s: a refusal must not carry a config object, got %#v", tc.name, kind, data)
			}
		}
	}
}

func TestAIPipelineStepCataloguesAreNotAdvertised(t *testing.T) {
	for name, h := range map[string]func(echo.Context) error{
		"preprocess":  handleListPreprocessSteps,
		"postprocess": handleListPostprocessSteps,
	} {
		code, ec, data := callAI(t, h, http.MethodGet, "/api/v1/ai/"+name+"/steps", "")
		if code != http.StatusNotImplemented {
			t.Fatalf("%s steps: status = %d, want 501", name, code)
		}
		if data != nil {
			t.Fatalf("%s steps: a refusal must not list step names, got %#v", name, data)
		}
		// The catalogue names nothing that the engine applies, so an answer that
		// still enumerates them (with any status) has to fail this test.
		want := "ERR_AI_"
		if len(ec) < len(want) || ec[:len(want)] != want {
			t.Fatalf("%s steps: error_code = %q, want an ERR_AI_* code", name, ec)
		}
	}
}
