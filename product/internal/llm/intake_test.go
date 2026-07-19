package llm

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIntakeLangDirective(t *testing.T) {
	if got := intakeLangDirective("sv"); !strings.Contains(got, "Swedish") {
		t.Errorf("sv: %q", got)
	}
	if got := intakeLangDirective("ru"); !strings.Contains(got, "Russian") {
		t.Errorf("ru: %q", got)
	}
	for _, code := range []string{"en", "", "xx"} {
		if got := intakeLangDirective(code); got != "" {
			t.Errorf("%q should yield no directive, got %q", code, got)
		}
	}
}

func TestConceptLangDirective(t *testing.T) {
	if got := conceptLangDirective("sv"); !strings.Contains(got, "Swedish") {
		t.Errorf("sv: %q", got)
	}
	if got := conceptLangDirective("ru"); !strings.Contains(got, "Russian") {
		t.Errorf("ru: %q", got)
	}
	if got := conceptLangDirective("en"); got != "" {
		t.Errorf("en should yield no directive, got %q", got)
	}
}

func TestParseIntakeJSONSanitizesVisualTiles(t *testing.T) {
	out := `{"questions":["What matters?"],"design_options":[{"name":"Field notes","description":"Documentary","palette":["#f4f0e8","not-css","#171713","#c64a2e","#ffffff"],"display_font":"Fraunces","body_font":"Manrope","hero_layout":"asymmetric","image_style":"Natural light","signature":"Circular crop","boldness":"bold"}]}`
	got, err := parseIntakeJSON(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.DesignOptions) != 1 {
		t.Fatalf("options = %+v", got.DesignOptions)
	}
	d := got.DesignOptions[0]
	if d.HeroLayout != "asymmetric" || d.Boldness != "bold" || len(d.Palette) != 4 || d.Palette[0] != "#F4F0E8" {
		t.Fatalf("structured tile was not sanitized correctly: %+v", d)
	}
}

func TestParseConceptJSONRequiresTwoDistinctSafeConcepts(t *testing.T) {
	out := `{"concepts":[{"id":"model-choice","name":"A","rationale":"Reason A","headline":"Headline A","subhead":"Sub A","cta":"Start","layout":"split","palette":["#f4f0e8","#171713","bad","#ffffff"],"display_font":"Fraunces","body_font":"Manrope","image_direction":"Soft daylight series","signature":"Circle"},{"id":"also-model-choice","name":"B","rationale":"Reason B","headline":"Headline B","subhead":"Sub B","cta":"See more","layout":"split","palette":["#ffffff","#111111","#cc5500","#eeeeee"],"display_font":"Oswald","body_font":"Source Sans","image_direction":"Crisp studio series","signature":"Rule"}]}`
	got, err := parseConceptJSON(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Concepts) != 2 || got.Concepts[0].ID != "concept-a" || got.Concepts[1].ID != "concept-b" {
		t.Fatalf("concept IDs/count not controlled: %+v", got.Concepts)
	}
	if got.Concepts[0].Layout == got.Concepts[1].Layout {
		t.Fatalf("concepts must not render as the same composition: %+v", got.Concepts)
	}
	if len(got.Concepts[0].Palette) != 3 {
		t.Fatalf("unsafe palette value survived: %v", got.Concepts[0].Palette)
	}
}

// TestQuestions_SendsLanguageDirective: a Swedish customer's intake request
// carries the Swedish directive to the model, so the questions come back in
// Swedish (not English).
func TestQuestions_SendsLanguageDirective(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		content := `{\"questions\":[\"q?\"],\"design_options\":[]}`
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"` + content + `"}}]}`))
	}))
	defer srv.Close()

	if _, err := retryTestClient(srv.URL).Questions(context.Background(), "a bakery site", "sv"); err != nil {
		t.Fatalf("questions: %v", err)
	}
	if !strings.Contains(gotBody, "Swedish") {
		t.Errorf("request should carry the Swedish directive; body:\n%s", gotBody)
	}
}
