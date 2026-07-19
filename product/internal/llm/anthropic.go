package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/project"
)

// DefaultModel is used when no model is configured.
const DefaultModel = "claude-sonnet-4-6"

// Anthropic implements Planner and SafetyGate against the Anthropic Messages API.
type Anthropic struct {
	baseURL string
	apiKey  string
	model   string
	effort  string // reasoning effort (low..max) via output_config; "" = model default
	http    *http.Client
}

// NewAnthropic returns a real planner/gate. model may be empty for DefaultModel;
// effort ("" | low | medium | high | xhigh | max) sets the reasoning depth on
// the 4.6+/5 models (adaptive thinking; never budget_tokens).
func NewAnthropic(apiKey, model, effort string) *Anthropic {
	return NewAnthropicAt("https://api.anthropic.com/v1", apiKey, model, effort)
}

// NewAnthropicAt returns a Messages API client using a compatible gateway.
// OpenCode Zen exposes Claude models at <baseURL>/messages with the same
// request/response shape as Anthropic's native endpoint.
func NewAnthropicAt(baseURL, apiKey, model, effort string) *Anthropic {
	if model == "" {
		model = DefaultModel
	}
	return &Anthropic{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		model:   model,
		effort:  effort,
		http:    &http.Client{Timeout: 300 * time.Second},
	}
}

type anthropicReq struct {
	Model        string           `json:"model"`
	MaxTokens    int              `json:"max_tokens"`
	System       string           `json:"system,omitempty"`
	Messages     []anthropicMsg   `json:"messages"`
	OutputConfig *anthropicOutCfg `json:"output_config,omitempty"`
}

type anthropicOutCfg struct {
	Effort string `json:"effort,omitempty"`
}

type anthropicMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicResp struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func (a *Anthropic) complete(ctx context.Context, system, user string, maxTokens int) (string, error) {
	r := anthropicReq{
		Model:     a.model,
		MaxTokens: maxTokens,
		System:    system,
		Messages:  []anthropicMsg{{Role: "user", Content: user}},
	}
	if a.effort != "" {
		// Higher effort spends more thinking (billed as output) — give the
		// response room so a deep plan isn't truncated.
		r.OutputConfig = &anthropicOutCfg{Effort: a.effort}
		if maxTokens < 16000 {
			r.MaxTokens = 16000
		}
	}
	body, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		a.baseURL+"/messages", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", a.apiKey)
	// Zen accepts the provider SDK's API-key authentication; the bearer header
	// also makes the same client work with compatible gateways that use OAuth2.
	if !strings.Contains(a.baseURL, "api.anthropic.com") {
		req.Header.Set("authorization", "Bearer "+a.apiKey)
	}
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := a.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var parsed anthropicResp
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("anthropic: decode response (status %d): %w", resp.StatusCode, err)
	}
	if parsed.Error != nil {
		return "", fmt.Errorf("anthropic: %s: %s", parsed.Error.Type, parsed.Error.Message)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("anthropic: unexpected status %d", resp.StatusCode)
	}
	var sb strings.Builder
	for _, block := range parsed.Content {
		if block.Type == "text" {
			sb.WriteString(block.Text)
		}
	}
	return sb.String(), nil
}

// Questions implements Intake.
func (a *Anthropic) Questions(ctx context.Context, brief, lang string) (IntakeResult, error) {
	out, err := a.complete(ctx, IntakeSystemPrompt+intakeLangDirective(lang), brief, 1000)
	if err != nil {
		return IntakeResult{}, err
	}
	return parseIntakeJSON(out)
}

// Concepts implements the concrete hero-concept gate using the same model and
// selected planner profile as intake.
func (a *Anthropic) Concepts(ctx context.Context, brief, design, lang string) (ConceptResult, error) {
	user := "Customer brief:\n" + brief + "\n\nChosen design direction:\n" + design
	out, err := a.complete(ctx, ConceptSystemPrompt+conceptLangDirective(lang), user, 3000)
	if err != nil {
		return ConceptResult{}, err
	}
	return parseConceptJSON(out)
}

// Plan implements Planner.
func (a *Anthropic) Plan(ctx context.Context, brief string) (PlanResult, error) {
	out, err := a.complete(ctx, PlannerSystemPrompt, brief, 2000)
	if err != nil {
		return PlanResult{}, err
	}
	name, plan := splitNameLine(out)
	if name == "" {
		name = deriveName(brief)
	}
	return PlanResult{Name: name, Plan: plan}, nil
}

// Screen implements SafetyGate.
func (a *Anthropic) Screen(ctx context.Context, brief, plan string) (GateResult, error) {
	user := "Request:\n" + brief
	if plan != "" {
		user += "\n\nProposed plan:\n" + plan
	}
	out, err := a.complete(ctx, SafetySystemPrompt, user, 300)
	if err != nil {
		return GateResult{}, err
	}
	return parseVerdict(out)
}

func splitNameLine(out string) (name, plan string) {
	out = strings.TrimSpace(out)
	lines := strings.SplitN(out, "\n", 2)
	if len(lines) > 0 && strings.HasPrefix(strings.ToUpper(lines[0]), "NAME:") {
		name = strings.TrimSpace(lines[0][len("NAME:"):])
		if len(lines) == 2 {
			plan = strings.TrimSpace(lines[1])
		}
		return name, plan
	}
	return "", out
}

func parseVerdict(out string) (GateResult, error) {
	// Be tolerant of models that wrap JSON in prose or code fences.
	start := strings.Index(out, "{")
	end := strings.LastIndex(out, "}")
	if start < 0 || end < start {
		return GateResult{}, fmt.Errorf("safety gate: no JSON in response: %q", out)
	}
	var parsed struct {
		Verdict string `json:"verdict"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(out[start:end+1]), &parsed); err != nil {
		return GateResult{}, fmt.Errorf("safety gate: bad JSON: %w", err)
	}
	switch project.Verdict(strings.ToLower(strings.TrimSpace(parsed.Verdict))) {
	case project.VerdictAllow:
		return GateResult{Verdict: project.VerdictAllow, Reason: parsed.Reason}, nil
	case project.VerdictReject:
		return GateResult{Verdict: project.VerdictReject, Reason: parsed.Reason}, nil
	case project.VerdictEscalate:
		return GateResult{Verdict: project.VerdictEscalate, Reason: parsed.Reason}, nil
	default:
		// Unknown verdict → fail safe by escalating to a human.
		return GateResult{Verdict: project.VerdictEscalate, Reason: "Unrecognized verdict; escalated for human review."}, nil
	}
}
