package opencode

import (
	"net/http"
	"net/http/httptest"
	"strings"
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

// sse builds one SSE data line.
func sse(v string) string { return "data: " + v + "\n" }

// The live log must show the WHOLE build: subagent (child session) tool calls
// relayed — "task" followed by silence reads as a hang — and the main
// session's narration flushed as whole blocks, deduped across its growing
// snapshots. Completion stays gated to the main session: a child going idle
// must not end the build.
func TestConsume_RelaysSubagentsAndNarration(t *testing.T) {
	stream := strings.NewReader(
		// Main session narrates in growing snapshots (same part id).
		sse(`{"type":"message.part.updated","properties":{"part":{"id":"t1","sessionID":"main","type":"text","text":"Planning the"}}}`) +
			sse(`{"type":"message.part.updated","properties":{"part":{"id":"t1","sessionID":"main","type":"text","text":"Planning the CRM tables."}}}`) +
			// Main spawns a task; the narration block must flush BEFORE the tool line.
			sse(`{"type":"message.part.updated","properties":{"part":{"id":"tool1","sessionID":"main","type":"tool","tool":"task","state":{"status":"running"}}}}`) +
			// A child session works: its tool call is relayed, its text is not.
			sse(`{"type":"message.part.updated","properties":{"part":{"id":"tool2","sessionID":"child","type":"tool","tool":"write","state":{"status":"running"}}}}`) +
			sse(`{"type":"message.part.updated","properties":{"part":{"id":"t2","sessionID":"child","type":"text","text":"child narration stays out"}}}`) +
			// The child finishing must NOT complete the build.
			sse(`{"type":"session.idle","properties":{"sessionID":"child"}}`) +
			// Main's final message, then the real completion.
			sse(`{"type":"message.part.updated","properties":{"part":{"id":"t3","sessionID":"main","type":"text","text":"All done."}}}`) +
			sse(`{"type":"session.idle","properties":{"sessionID":"main"}}`))

	var lines []string
	h := NewHTTP("http://unused")
	final, err := h.consume("main", stream, func(l string) { lines = append(lines, l) })
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if final != "All done." {
		t.Errorf("final text = %q, want the main session's last message", final)
	}

	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "Planning the CRM tables.") {
		t.Errorf("main narration missing from log:\n%s", joined)
	}
	if strings.Count(joined, "Planning the") != 1 {
		t.Errorf("growing text snapshots must flush exactly once:\n%s", joined)
	}
	if !strings.Contains(joined, "All done.") {
		t.Errorf("final message missing from log:\n%s", joined)
	}
	if strings.Contains(joined, "child narration") {
		t.Errorf("child session text must stay out of the log:\n%s", joined)
	}

	// The child's tool line is relayed, and narration precedes the task line.
	var narrIdx, taskIdx, childToolIdx int = -1, -1, -1
	for i, l := range lines {
		switch {
		case strings.Contains(l, "Planning the CRM tables."):
			narrIdx = i
		case strings.Contains(l, "task"):
			taskIdx = i
		case strings.Contains(l, "write"):
			childToolIdx = i
		}
	}
	if childToolIdx == -1 {
		t.Errorf("subagent tool call not relayed:\n%s", joined)
	}
	if narrIdx == -1 || taskIdx == -1 || narrIdx > taskIdx {
		t.Errorf("narration should flush before the task tool line; lines:\n%s", joined)
	}
}

// A child session erroring is the parent's problem to handle — the build keeps
// consuming until the MAIN session resolves.
func TestConsume_ChildErrorDoesNotFailBuild(t *testing.T) {
	stream := strings.NewReader(
		sse(`{"type":"session.error","properties":{"sessionID":"child","error":{"boom":true}}}`) +
			sse(`{"type":"message.part.updated","properties":{"part":{"id":"t1","sessionID":"main","type":"text","text":"recovered"}}}`) +
			sse(`{"type":"session.idle","properties":{"sessionID":"main"}}`))

	h := NewHTTP("http://unused")
	final, err := h.consume("main", stream, nil)
	if err != nil {
		t.Fatalf("a child error must not fail the build: %v", err)
	}
	if final != "recovered" {
		t.Errorf("final = %q, want %q", final, "recovered")
	}
}
