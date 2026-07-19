package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestComplete_ResponsesProtocol(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = w.Write([]byte(`{"output":[{"content":[{"type":"output_text","text":"NAME: Zen site\nA complete plan"}]}]}`))
	}))
	defer srv.Close()

	c := NewOpenAICompat(srv.URL, "zen-key", "gpt-5.6-sol").WithEffort("high").WithProtocol("responses")
	res, err := c.Plan(context.Background(), "build a site")
	if err != nil {
		t.Fatalf("responses plan: %v", err)
	}
	if gotPath != "/responses" {
		t.Errorf("path = %q, want /responses", gotPath)
	}
	if gotBody["model"] != "gpt-5.6-sol" || gotBody["instructions"] == "" || gotBody["input"] != "build a site" {
		t.Errorf("unexpected responses request: %#v", gotBody)
	}
	reasoning, _ := gotBody["reasoning"].(map[string]any)
	if reasoning["effort"] != "high" {
		t.Errorf("reasoning = %#v, want high effort", reasoning)
	}
	if res.Name != "Zen site" || res.Plan != "A complete plan" {
		t.Errorf("unexpected plan: %+v", res)
	}
}

func TestAnthropicAt_MessagesGateway(t *testing.T) {
	var gotPath, gotAPIKey, gotBearer string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAPIKey = r.Header.Get("x-api-key")
		gotBearer = r.Header.Get("authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"NAME: Zen Claude\nA complete plan"}]}`))
	}))
	defer srv.Close()

	res, err := NewAnthropicAt(srv.URL, "zen-key", "claude-sonnet-5", "max").Plan(context.Background(), "build a site")
	if err != nil {
		t.Fatalf("messages plan: %v", err)
	}
	if gotPath != "/messages" || gotAPIKey != "zen-key" || gotBearer != "Bearer zen-key" {
		t.Errorf("request path/auth = %q, %q, %q", gotPath, gotAPIKey, gotBearer)
	}
	if gotBody["model"] != "claude-sonnet-5" {
		t.Errorf("unexpected messages request: %#v", gotBody)
	}
	if res.Name != "Zen Claude" || res.Plan != "A complete plan" {
		t.Errorf("unexpected plan: %+v", res)
	}
}

// retryTestClient returns a client pointed at url with a near-zero retry delay.
func retryTestClient(url string) *OpenAICompat {
	c := NewOpenAICompat(url, "test-key", "test-model")
	c.retryDelay = time.Millisecond
	return c
}

func TestComplete_RetriesTransientThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "upstream hiccup", http.StatusBadGateway) // 502 → retryable
			return
		}
		content := `{\"questions\":[\"q1?\"],\"design_options\":[{\"name\":\"Clean\",\"description\":\"minimal\"}]}`
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"` + content + `"}}]}`))
	}))
	defer srv.Close()

	res, err := retryTestClient(srv.URL).Questions(context.Background(), "a bakery site", "en")
	if err != nil {
		t.Fatalf("expected success after one retry, got %v", err)
	}
	if len(res.Questions) != 1 || res.Questions[0] != "q1?" {
		t.Fatalf("unexpected questions: %v", res.Questions)
	}
	if len(res.DesignOptions) != 1 || res.DesignOptions[0].Name != "Clean" {
		t.Fatalf("unexpected design options: %v", res.DesignOptions)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("expected exactly 2 calls (fail + retry), got %d", got)
	}
}

func TestComplete_DoesNotRetryPermanent(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "bad request", http.StatusBadRequest) // 400 → permanent
	}))
	defer srv.Close()

	_, err := retryTestClient(srv.URL).Plan(context.Background(), "a bakery site")
	if err == nil {
		t.Fatal("expected an error for a 400")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("a 400 must not be retried; expected 1 call, got %d", got)
	}
}

func TestComplete_StopsAfterMaxAttempts(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "still down", http.StatusServiceUnavailable) // 503 → retryable, always
	}))
	defer srv.Close()

	_, err := retryTestClient(srv.URL).Screen(context.Background(), "a bakery site", "")
	if err == nil {
		t.Fatal("expected an error when the endpoint never recovers")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("expected exactly maxAttempts (2) calls, got %d", got)
	}
}
