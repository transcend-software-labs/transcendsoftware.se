package opencode

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// totalTokens is Attach's liveness signal and must span every session — a
// build's polish pass runs in a second session, and counting only the attached
// one made the watchdog finalise under a still-working agent.
func TestTotalTokens_SumsAllSessions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/session" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`[
			{"id":"ses_main","tokens":{"input":100,"output":40,"reasoning":10}},
			{"id":"ses_polish","tokens":{"input":7,"output":2,"reasoning":1}}
		]`))
	}))
	defer srv.Close()

	h := NewHTTP(srv.URL)
	if got := h.totalTokens(t.Context()); got != 160 {
		t.Fatalf("totalTokens = %d, want 160 (both sessions)", got)
	}
}

// A dead server must read as "no growth", not an error — the watchdog treats a
// frozen count as finished, which is the right call for an unreachable sandbox.
func TestTotalTokens_UnreachableIsZero(t *testing.T) {
	h := NewHTTP("http://127.0.0.1:1")
	if got := h.totalTokens(t.Context()); got != 0 {
		t.Fatalf("totalTokens on unreachable server = %d, want 0", got)
	}
}
