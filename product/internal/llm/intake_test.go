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
