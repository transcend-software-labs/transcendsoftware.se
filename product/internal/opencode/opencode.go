// Package opencode drives an opencode server over its HTTP API to run a build.
//
// In dev mode the Fake driver simulates a build. The HTTP driver targets a real
// opencode server (one per sandboxed task). The exact endpoint/event shapes vary
// by opencode version — the HTTP driver is a thin, clearly-marked wrapper to be
// confirmed against the server you run (see opencode.ai/docs/server).
package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
}

// Driver runs a build via opencode.
type Driver interface {
	Run(ctx context.Context, spec Spec) (Result, error)
}

// Fake is a deterministic Driver for dev mode; it performs no real work.
type Fake struct{}

// NewFake returns a dev-mode driver.
func NewFake() *Fake { return &Fake{} }

func (Fake) Run(ctx context.Context, spec Spec) (Result, error) {
	// Simulate a short build so the dashboard shows realistic progression.
	select {
	case <-time.After(1500 * time.Millisecond):
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
	log := "[dev] agent scaffolded the site\n[dev] installed dependencies\n[dev] build passed\n" +
		"[dev] instruction was:\n" + strings.TrimSpace(spec.Instruction)
	return Result{Log: log, SessionID: "dev-session"}, nil
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

func (h *HTTP) Run(ctx context.Context, spec Spec) (Result, error) {
	// 1) Create a session.
	sessionID, err := h.createSession(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("opencode: create session: %w", err)
	}
	// 2) Send the instruction and collect the response.
	log, err := h.sendMessage(ctx, sessionID, spec.SystemPrompt+"\n\n"+spec.Instruction)
	if err != nil {
		return Result{}, fmt.Errorf("opencode: run: %w", err)
	}
	return Result{Log: log, SessionID: sessionID}, nil
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

func (h *HTTP) sendMessage(ctx context.Context, sessionID, text string) (string, error) {
	var out struct {
		Parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"parts"`
	}
	body := map[string]any{
		"parts": []map[string]any{{"type": "text", "text": text}},
	}
	if err := h.postJSON(ctx, "/session/"+sessionID+"/message", body, &out); err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, p := range out.Parts {
		if p.Type == "text" {
			sb.WriteString(p.Text)
		}
	}
	return sb.String(), nil
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
