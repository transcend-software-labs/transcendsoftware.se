// Package opencode drives an opencode server over its HTTP API to run a build.
//
// In dev mode the Fake driver simulates a build. The HTTP driver targets a real
// opencode server (one per sandboxed task). The exact endpoint/event shapes vary
// by opencode version — the HTTP driver is a thin, clearly-marked wrapper to be
// confirmed against the server you run (see opencode.ai/docs/server).
package opencode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Spec describes one build run for the agent.
type Spec struct {
	Workdir      string // working directory inside the sandbox
	SystemPrompt string // "Rasmus's decisions" operating spec
	Instruction  string // the concrete task (plan, or a reiteration prompt)
}

// Result is the outcome of a build run.
type Result struct {
	Log       string // streamed/aggregated agent output
	SessionID string // opencode session id, for resuming reiterations
	Tokens    int    // total model tokens the build consumed (0 if unknown)
}

// Driver runs a build via opencode. onLog, if non-nil, receives progress lines
// as they happen (for live streaming to the dashboard). onSession, if non-nil,
// is called once with the session id as soon as it exists — the orchestrator
// persists it so a restart can re-attach to the still-running build.
type Driver interface {
	Run(ctx context.Context, spec Spec, onLog func(string), onSession func(string)) (Result, error)
	// Attach re-connects to a build already running under sessionID (its opencode
	// async session survives the orchestrator dying) and consumes the rest of its
	// event stream to completion. No new session, no re-prompt. The build's
	// remaining deadline comes from ctx.
	Attach(ctx context.Context, sessionID string, onLog func(string)) (Result, error)
}

// Fake is a deterministic Driver for dev mode; it performs no real work.
type Fake struct{}

// NewFake returns a dev-mode driver.
func NewFake() *Fake { return &Fake{} }

func (Fake) Run(ctx context.Context, spec Spec, onLog func(string), onSession func(string)) (Result, error) {
	if onSession != nil {
		onSession("dev-session")
	}
	// Simulate a build, emitting progress lines over time so the live log streams.
	lines := []string{
		"Spawning isolated sandbox…",
		"Cloning workspace…",
		"Agent reading the plan…",
		"Scaffolding the site…",
		"Installing dependencies…",
		"Running the verifier (build + tests)…",
		"Build passed ✓",
		"Deploying preview…",
	}
	var all []string
	for _, ln := range lines {
		select {
		case <-time.After(300 * time.Millisecond):
		case <-ctx.Done():
			return Result{Log: strings.Join(all, "\n")}, ctx.Err()
		}
		all = append(all, ln)
		if onLog != nil {
			onLog(ln)
		}
	}
	return Result{Log: strings.Join(all, "\n"), SessionID: "dev-session"}, nil
}

// Attach simulates re-connecting to a running dev build (finishes immediately).
func (Fake) Attach(ctx context.Context, sessionID string, onLog func(string)) (Result, error) {
	if onLog != nil {
		onLog("Reconnected to the running build…")
	}
	return Result{Log: "reattached", SessionID: sessionID}, nil
}

// HTTP drives a real opencode server at BaseURL. Confirm endpoints against your
// opencode version before relying on this in production.
type HTTP struct {
	BaseURL string
	client  *http.Client
}

// NewHTTP returns a driver targeting the opencode server at baseURL.
func NewHTTP(baseURL string) *HTTP {
	return &HTTP{
		BaseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Timeout: 30 * time.Minute},
	}
}

// BuildDeadline caps a single build run. Real multi-page sites — where the
// agent writes several pages, fetches and processes images, and runs the test
// build a few times — routinely need more than half an hour on Kimi (slow at
// temperature 1). Kept comfortably under the orchestrator's pipelineTimeout.
// Exported so a re-attach after a restart can bound the resumed run by the same
// cap, measured from the original build's start.
const BuildDeadline = 90 * time.Minute

func (h *HTTP) Run(ctx context.Context, spec Spec, onLog func(string), onSession func(string)) (Result, error) {
	sessionID, err := h.createSession(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("opencode: create session: %w", err)
	}
	emit(onLog, "opencode session started")
	// Hand the session id back immediately so the orchestrator can persist it —
	// before the agent runs, so a restart mid-build can re-attach to it.
	if onSession != nil {
		onSession(sessionID)
	}

	streamCtx, cancel := context.WithTimeout(ctx, BuildDeadline)
	defer cancel()

	// Open the event stream BEFORE prompting so we don't miss early tool activity.
	stream, err := h.openEvents(streamCtx)
	if err != nil {
		return Result{}, fmt.Errorf("opencode: open event stream: %w", err)
	}
	defer stream.Close()

	// Fire the prompt asynchronously; progress arrives over the event stream.
	body := map[string]any{
		"parts": []map[string]any{{"type": "text", "text": spec.SystemPrompt + "\n\n" + spec.Instruction}},
	}
	if err := h.postJSON(ctx, "/session/"+sessionID+"/prompt_async", body, nil); err != nil {
		return Result{}, fmt.Errorf("opencode: prompt: %w", err)
	}

	log, err := h.consume(sessionID, stream, onLog)
	// Best-effort token accounting from the session (for cost visibility).
	tokens := 0
	if err == nil {
		tokens = h.sessionTokens(ctx, sessionID)
	}
	return Result{Log: log, SessionID: sessionID, Tokens: tokens}, err
}

// Attach re-connects to a build already running under sessionID and consumes
// the rest of its event stream to completion. The opencode session runs
// server-side and is unaffected by the orchestrator restarting, so this simply
// re-opens the /event stream and waits for the same session.idle it would have
// waited for originally. The remaining build deadline is carried by ctx.
//
// Caveat: opencode's /event stream is live, not replayed. If the agent happened
// to finish during the exact window the orchestrator was down, session.idle was
// already emitted and won't repeat — consume then blocks until ctx's deadline,
// after which the caller saves a snapshot and the build resumes via Retry. That
// window is ~seconds; the common case (agent still working) re-attaches cleanly.
func (h *HTTP) Attach(ctx context.Context, sessionID string, onLog func(string)) (Result, error) {
	emit(onLog, "Reconnected to the running build…")
	stream, err := h.openEvents(ctx)
	if err != nil {
		return Result{SessionID: sessionID}, fmt.Errorf("opencode: reattach event stream: %w", err)
	}
	defer stream.Close()
	log, err := h.consume(sessionID, stream, onLog)
	tokens := 0
	if err == nil {
		tokens = h.sessionTokens(ctx, sessionID)
	}
	return Result{Log: log, SessionID: sessionID, Tokens: tokens}, err
}

// sessionTokens reads the session's token totals (input + output + reasoning),
// best-effort — a failure just yields 0.
func (h *HTTP) sessionTokens(ctx context.Context, sessionID string) int {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.BaseURL+"/session/"+sessionID, nil)
	if err != nil {
		return 0
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var s struct {
		Tokens struct {
			Input     int `json:"input"`
			Output    int `json:"output"`
			Reasoning int `json:"reasoning"`
		} `json:"tokens"`
	}
	if json.Unmarshal(raw, &s) != nil {
		return 0
	}
	return s.Tokens.Input + s.Tokens.Output + s.Tokens.Reasoning
}

func (h *HTTP) createSession(ctx context.Context) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	if err := h.postJSON(ctx, "/session", map[string]any{}, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// ocEvent is one server-sent event from opencode's /event stream.
type ocEvent struct {
	Type       string `json:"type"`
	Properties struct {
		SessionID string          `json:"sessionID"`
		Part      ocEventPart     `json:"part"`
		Error     json.RawMessage `json:"error"` // session.error payload — surfaced for diagnosability
	} `json:"properties"`
}

type ocEventPart struct {
	ID        string          `json:"id"`
	SessionID string          `json:"sessionID"`
	Type      string          `json:"type"`
	Tool      string          `json:"tool"`
	Text      string          `json:"text"`
	State     json.RawMessage `json:"state"`
}

// openEvents starts reading opencode's SSE /event stream. The returned Closer
// must be closed by the caller.
func (h *HTTP) openEvents(ctx context.Context) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.BaseURL+"/event", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("accept", "text/event-stream")
	// Streaming request: no overall client timeout (lifetime is bounded by ctx),
	// but cap the connect so a dead/unreachable sandbox — e.g. when re-attaching
	// to a machine that was reaped — fails fast instead of hanging on the deadline.
	client := &http.Client{Transport: &http.Transport{
		DialContext: (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
	}}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("opencode: /event returned %d: %s", resp.StatusCode, string(raw))
	}
	return resp.Body, nil
}

// consume reads events for sessionID, streaming each tool action via onLog and
// returning the final assistant text once the session goes idle.
func (h *HTTP) consume(sessionID string, stream io.Reader, onLog func(string)) (string, error) {
	seen := map[string]bool{}
	var lastText string
	sc := bufio.NewScanner(stream)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024) // tool payloads can be large
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var ev ocEvent
		if json.Unmarshal([]byte(strings.TrimSpace(line[5:])), &ev) != nil {
			continue
		}
		sid := ev.Properties.SessionID
		if sid == "" {
			sid = ev.Properties.Part.SessionID
		}
		if sid != sessionID {
			continue
		}
		switch ev.Type {
		case "message.part.updated":
			p := ev.Properties.Part
			switch p.Type {
			case "tool":
				// Emit once, after the input is populated (status past "pending").
				if !seen[p.ID] && partStatus(p.State) != "pending" {
					seen[p.ID] = true
					emit(onLog, toolLine(p.Tool, p.State))
				}
			case "text":
				if p.Text != "" {
					lastText = p.Text
				}
			}
		case "session.idle":
			if lastText != "" {
				emit(onLog, lastText)
			}
			return lastText, nil
		case "session.error":
			// Surface the actual error payload — "agent reported an error" alone
			// made model/provider failures (401s, unknown models) undiagnosable.
			detail := strings.TrimSpace(string(ev.Properties.Error))
			if len(detail) > 500 {
				detail = detail[:500] + "…"
			}
			if detail == "" || detail == "null" {
				return lastText, fmt.Errorf("opencode: agent reported an error")
			}
			return lastText, fmt.Errorf("opencode: agent reported an error: %s", detail)
		}
	}
	if err := sc.Err(); err != nil {
		return lastText, fmt.Errorf("opencode: event stream error: %w", err)
	}
	return lastText, fmt.Errorf("opencode: event stream ended before the build finished")
}

func partStatus(state json.RawMessage) string {
	var s struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(state, &s)
	return s.Status
}

// toolLine summarises a tool action as a progress line, e.g. "→ write index.html".
func toolLine(tool string, state json.RawMessage) string {
	desc := tool
	if desc == "" {
		desc = "tool"
	}
	var st struct {
		Input map[string]any `json:"input"`
	}
	if json.Unmarshal(state, &st) == nil {
		if fp, ok := st.Input["filePath"].(string); ok {
			desc += " " + fp
		} else if cmd, ok := st.Input["command"].(string); ok {
			desc += ": " + truncate(cmd, 70)
		}
	}
	return "→ " + desc
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func emit(onLog func(string), line string) {
	if onLog != nil {
		onLog(line)
	}
}

func (h *HTTP) postJSON(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("opencode: %s returned %d: %s", path, resp.StatusCode, string(raw))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}
