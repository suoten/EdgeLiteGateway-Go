package engine

import (
	"testing"
)

// The inference API accepts several payload shapes: a bare numeric array under
// "input_data", the {"values":[...]} wrapper simulators send, and nested maps.
// normalizeInput used to drop every array payload with "no numeric values in
// input" (500), which is why these shapes are pinned here.

func TestNormalizeInputBareArray(t *testing.T) {
	got, err := normalizeInput(map[string]interface{}{
		"input_data": []interface{}{20.0, 22.0, 25.0},
	})
	if err != nil {
		t.Fatalf("bare array rejected: %v", err)
	}
	if len(got) != 3 || got[0] != 20 || got[2] != 25 {
		t.Fatalf("got %v", got)
	}
}

func TestNormalizeInputValuesWrapper(t *testing.T) {
	got, err := normalizeInput(map[string]interface{}{
		"input_data": map[string]interface{}{
			"values": []interface{}{1.0, 2.0, 3.0, 4.0},
		},
	})
	if err != nil {
		t.Fatalf("values wrapper rejected: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("got %v", got)
	}
}

func TestNormalizeInputFlatNumericMap(t *testing.T) {
	got, err := normalizeInput(map[string]interface{}{
		"v1": 10.0,
		"v2": 20.0,
	})
	if err != nil {
		t.Fatalf("flat map rejected: %v", err)
	}
	sum := got[0] + got[1]
	if sum != 30 {
		t.Fatalf("got %v", got)
	}
}

func TestNormalizeInputNoNumbersIsAnError(t *testing.T) {
	if _, err := normalizeInput(map[string]interface{}{"name": "abc"}); err == nil {
		t.Fatal("non-numeric input must be an error")
	}
}
