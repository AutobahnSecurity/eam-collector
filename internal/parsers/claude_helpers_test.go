package parsers

import (
	"testing"
)

func TestRestoreOffsets_Normal(t *testing.T) {
	// Simulates what JSON unmarshal produces: map[string]any with float64 values
	prevState := map[string]any{
		"file_offsets": map[string]any{
			"a": 100.0,
			"b": 200.0,
		},
	}
	got := restoreOffsets(prevState)
	if got["a"] != 100.0 {
		t.Errorf("offsets[a] = %f, want 100", got["a"])
	}
	if got["b"] != 200.0 {
		t.Errorf("offsets[b] = %f, want 200", got["b"])
	}
}

func TestRestoreOffsets_DirectFloat64Map(t *testing.T) {
	// The code also handles the case where the value is already map[string]float64
	prevState := map[string]any{
		"file_offsets": map[string]float64{
			"c": 300.0,
		},
	}
	got := restoreOffsets(prevState)
	if got["c"] != 300.0 {
		t.Errorf("offsets[c] = %f, want 300", got["c"])
	}
}

func TestRestoreOffsets_EmptyState(t *testing.T) {
	got := restoreOffsets(map[string]any{})
	if len(got) != 0 {
		t.Errorf("got %d entries, want 0", len(got))
	}
}

func TestRestoreOffsets_NilState(t *testing.T) {
	got := restoreOffsets(nil)
	if got == nil {
		t.Error("restoreOffsets(nil) returned nil, want empty map")
	}
	if len(got) != 0 {
		t.Errorf("got %d entries, want 0", len(got))
	}
}

func TestRestoreOffsets_MissingKey(t *testing.T) {
	prevState := map[string]any{
		"other_key": "value",
	}
	got := restoreOffsets(prevState)
	if len(got) != 0 {
		t.Errorf("got %d entries, want 0", len(got))
	}
}

func TestRestoreOffsets_NonFloatValuesSkipped(t *testing.T) {
	prevState := map[string]any{
		"file_offsets": map[string]any{
			"good": 100.0,
			"bad":  "not a number",
		},
	}
	got := restoreOffsets(prevState)
	if got["good"] != 100.0 {
		t.Errorf("offsets[good] = %f, want 100", got["good"])
	}
	if _, exists := got["bad"]; exists {
		t.Error("offsets[bad] should not exist (non-float value)")
	}
}

func TestRestoreKnownFiles_Normal(t *testing.T) {
	prevState := map[string]any{
		"known_files": map[string]any{
			"a": true,
			"b": true,
		},
	}
	got := restoreKnownFiles(prevState)
	if !got["a"] {
		t.Error("known[a] should be true")
	}
	if !got["b"] {
		t.Error("known[b] should be true")
	}
}

func TestRestoreKnownFiles_DirectBoolMap(t *testing.T) {
	prevState := map[string]any{
		"known_files": map[string]bool{
			"c": true,
			"d": false,
		},
	}
	got := restoreKnownFiles(prevState)
	if !got["c"] {
		t.Error("known[c] should be true")
	}
	if got["d"] {
		t.Error("known[d] should be false (false values not stored)")
	}
}

func TestRestoreKnownFiles_EmptyState(t *testing.T) {
	got := restoreKnownFiles(map[string]any{})
	if len(got) != 0 {
		t.Errorf("got %d entries, want 0", len(got))
	}
}

func TestRestoreKnownFiles_NilState(t *testing.T) {
	got := restoreKnownFiles(nil)
	if got == nil {
		t.Error("restoreKnownFiles(nil) returned nil, want empty map")
	}
	if len(got) != 0 {
		t.Errorf("got %d entries, want 0", len(got))
	}
}

func TestRestoreKnownFiles_FalseValuesExcluded(t *testing.T) {
	prevState := map[string]any{
		"known_files": map[string]any{
			"present": true,
			"absent":  false,
		},
	}
	got := restoreKnownFiles(prevState)
	if !got["present"] {
		t.Error("known[present] should be true")
	}
	if got["absent"] {
		t.Error("known[absent] should not be set (false value)")
	}
}

func TestRestoreKnownFiles_NonBoolValuesSkipped(t *testing.T) {
	prevState := map[string]any{
		"known_files": map[string]any{
			"good": true,
			"bad":  "not a bool",
		},
	}
	got := restoreKnownFiles(prevState)
	if !got["good"] {
		t.Error("known[good] should be true")
	}
	if _, exists := got["bad"]; exists {
		t.Error("known[bad] should not exist (non-bool value)")
	}
}
