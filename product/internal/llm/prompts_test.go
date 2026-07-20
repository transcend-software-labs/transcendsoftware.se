package llm

import (
	"strings"
	"testing"
)

// The intake prompt's JSON example once taught the exact cream/rust/serif
// cluster the pipeline warns against — few-shot examples outweigh prose, so
// guard against reintroducing those values.
func TestIntakePromptExampleIsNotTheOverusedCluster(t *testing.T) {
	for _, banned := range []string{"#F4F0E8", "#C64A2E", `"display_font":"Fraunces"`} {
		if strings.Contains(IntakeSystemPrompt, banned) {
			t.Errorf("IntakeSystemPrompt reintroduces overused cluster value %q", banned)
		}
	}
}

// The look is chosen at intake, so the diversity guardrails must live in the
// intake prompt (not only in the later planner prompt).
func TestIntakePromptCarriesDiversityGuardrails(t *testing.T) {
	for _, want := range []string{"AT LEAST THREE", "axes", "acid-green", "Purple/violet"} {
		if !strings.Contains(IntakeSystemPrompt, want) {
			t.Errorf("IntakeSystemPrompt missing diversity guardrail %q", want)
		}
	}
}

func TestConceptPromptCarriesAntiDefaultGuardrail(t *testing.T) {
	if !strings.Contains(ConceptSystemPrompt, "acid-green") {
		t.Error("ConceptSystemPrompt missing the anti-default guardrail")
	}
}

// FallbackDesignOptions ship to customers when the intake model omits its
// design array — they must not collapse into one look, nor into the cluster.
func TestFallbackDesignOptionsAreDistinctWorlds(t *testing.T) {
	opts := FallbackDesignOptions()
	if len(opts) != 2 {
		t.Fatalf("expected 2 fallback options, got %+v", opts)
	}
	a, b := opts[0], opts[1]
	if a.Palette[0] == b.Palette[0] || a.Palette[2] == b.Palette[2] {
		t.Errorf("fallback palettes must differ on background and accent: %+v vs %+v", a.Palette, b.Palette)
	}
	if a.DisplayFont == b.DisplayFont {
		t.Errorf("fallback display fonts must differ, both are %q", a.DisplayFont)
	}
	for _, banned := range []string{"#F2EBDD", "#B9472D"} {
		for _, o := range opts {
			for _, hex := range o.Palette {
				if strings.EqualFold(hex, banned) {
					t.Errorf("fallback %q reintroduces overused cluster value %q", o.Name, banned)
				}
			}
		}
	}
}
