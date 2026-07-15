package web

import "testing"

// TestCombineAnswers: the per-question fields are paired back to their questions
// into one readable block, blanks skipped, and never panics on length mismatch.
func TestCombineAnswers(t *testing.T) {
	qs := []string{"What is it called?", "Demo or real app?", "Languages?"}

	// Normal: three questions, middle-and-last answered, one blank skipped.
	got := combineAnswers(qs, []string{"Acme CRM", "", "English + Swedish"})
	want := "What is it called?\n→ Acme CRM\n\nLanguages?\n→ English + Swedish"
	if got != want {
		t.Errorf("combineAnswers pairing:\n got %q\nwant %q", got, want)
	}

	// Trims whitespace per answer.
	if got := combineAnswers([]string{"Q?"}, []string{"  hi  "}); got != "Q?\n→ hi" {
		t.Errorf("expected trimmed answer, got %q", got)
	}

	// No questions → empty (design-only intake).
	if got := combineAnswers(nil, []string{"stray"}); got != "" {
		t.Errorf("no questions should yield empty, got %q", got)
	}

	// Fewer answers than questions must not panic and skips the missing one.
	if got := combineAnswers(qs, []string{"only one"}); got != "What is it called?\n→ only one" {
		t.Errorf("short answers slice: got %q", got)
	}

	// All blank → empty (caller treats as "no answers").
	if got := combineAnswers(qs, []string{"", "", ""}); got != "" {
		t.Errorf("all-blank should yield empty, got %q", got)
	}
}
