package anthropic

import "testing"

func TestIsSyntheticEmptyEcho(t *testing.T) {
	tests := []struct {
		name string
		text string
		want bool
	}{
		{"current placeholder", SyntheticEmptyText, true},
		{"legacy placeholder", legacySyntheticEmptyText, true},
		{"legacy with surrounding whitespace", "  (empty)\n", true},
		{"current with trailing newline", SyntheticEmptyText + "\n", true},
		{"empty string", "", false},
		{"whitespace only", "   \n", false},
		{"real answer", "The build passes.", false},
		{"placeholder followed by real content", SyntheticEmptyText + " and here is the answer", false},
		{"legacy embedded mid-sentence", "the normalizer injects (empty) between turns", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsSyntheticEmptyEcho(tt.text); got != tt.want {
				t.Errorf("IsSyntheticEmptyEcho(%q) = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}

func TestMayBeSyntheticEmptyEcho(t *testing.T) {
	tests := []struct {
		name string
		text string
		want bool
	}{
		{"full current placeholder", SyntheticEmptyText, true},
		{"full legacy placeholder", legacySyntheticEmptyText, true},
		{"prefix of current placeholder", SyntheticEmptyText[:5], true},
		{"prefix of legacy placeholder", "(emp", true},
		{"single opening bracket", "[", true},
		{"empty string", "", false},
		{"diverges from every placeholder", "The build passes.", false},
		{"placeholder plus more content", SyntheticEmptyText + " x", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MayBeSyntheticEmptyEcho(tt.text); got != tt.want {
				t.Errorf("MayBeSyntheticEmptyEcho(%q) = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}

// The placeholder is injected into history on nearly every multi-turn request,
// so a value the model reads as a plausible reply is one it will imitate. Guard
// the shape that makes it read as an annotation instead.
func TestSyntheticEmptyText_ReadsAsAnnotation(t *testing.T) {
	if got := SyntheticEmptyText; got[0] != '[' || got[len(got)-1] != ']' {
		t.Errorf("SyntheticEmptyText = %q, want a bracketed annotation", got)
	}
	if SyntheticEmptyText == legacySyntheticEmptyText {
		t.Error("SyntheticEmptyText must differ from the legacy value it replaces")
	}
}
