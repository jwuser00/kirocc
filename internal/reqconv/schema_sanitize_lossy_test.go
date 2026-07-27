package reqconv

import "testing"

// threeBranchAnyOf builds a schema whose anyOf cannot be flattened (no enums,
// more than one non-null branch), which is the shape that triggers the lossy
// first-branch conversion.
func threeBranchAnyOf(firstType string) map[string]any {
	return map[string]any{
		"anyOf": []any{
			map[string]any{"type": firstType},
			map[string]any{"type": "number"},
			map[string]any{"type": "boolean"},
		},
	}
}

func TestSanitizeJSONSchema_LossyAnyOfUsesFirstBranch(t *testing.T) {
	got := SanitizeJSONSchema(threeBranchAnyOf("string"))
	if got["type"] != "string" {
		t.Errorf("type = %v, want %q (first branch)", got["type"], "string")
	}
	if _, ok := got["anyOf"]; ok {
		t.Error("anyOf should not survive sanitization")
	}
}

func TestFirstSighting_DedupesRepeatedSchemas(t *testing.T) {
	resetLossyWarnState(t)

	branches := threeBranchAnyOf("string")["anyOf"].([]any)

	if !firstSighting("anyOf", branches) {
		t.Fatal("first sighting of a schema should report true")
	}
	for i := range 5 {
		if firstSighting("anyOf", branches) {
			t.Errorf("repeat %d of the same schema reported as first sighting", i+1)
		}
	}
}

func TestFirstSighting_DistinctSchemasEachWarnOnce(t *testing.T) {
	resetLossyWarnState(t)

	a := threeBranchAnyOf("string")["anyOf"].([]any)
	b := threeBranchAnyOf("integer")["anyOf"].([]any)

	if !firstSighting("anyOf", a) {
		t.Error("schema A should report a first sighting")
	}
	if !firstSighting("anyOf", b) {
		t.Error("schema B differs from A and should report its own first sighting")
	}
	if firstSighting("oneOf", a) != true {
		t.Error("same branches under a different combinator should report separately")
	}
	if firstSighting("anyOf", a) {
		t.Error("schema A repeat should be deduped")
	}
}

func TestFirstSighting_StopsRecordingAtCap(t *testing.T) {
	resetLossyWarnState(t)

	// Fill the table to the cap with distinct schemas.
	for i := range lossyWarnCap {
		branches := []any{
			map[string]any{"type": "string", "title": i},
			map[string]any{"type": "number"},
			map[string]any{"type": "boolean"},
		}
		if !firstSighting("anyOf", branches) {
			t.Fatalf("schema %d below the cap should report a first sighting", i)
		}
	}

	overflow := []any{
		map[string]any{"type": "string", "title": "overflow"},
		map[string]any{"type": "number"},
		map[string]any{"type": "boolean"},
	}
	if firstSighting("anyOf", overflow) {
		t.Error("schema past the cap must not report a first sighting")
	}
}

// resetLossyWarnState clears the dedupe table so tests do not leak state into
// each other, and restores it when the test finishes.
func resetLossyWarnState(t *testing.T) {
	t.Helper()
	clear := func() {
		lossyWarnSeen.Range(func(k, _ any) bool {
			lossyWarnSeen.Delete(k)
			return true
		})
		lossyWarnCount.mu.Lock()
		lossyWarnCount.n = 0
		lossyWarnCount.mu.Unlock()
	}
	clear()
	t.Cleanup(clear)
}
