package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

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
