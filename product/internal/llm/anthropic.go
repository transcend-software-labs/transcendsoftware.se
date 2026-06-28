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
	apiKey string
	model  string
	http   *http.Client
}

// NewAnthropic returns a real planner/gate. model may be empty for DefaultModel.
func NewAnthropic(apiKey, model string) *Anthropic {
	if model == "" {
		model = DefaultModel
	}
	return &Anthropic{
		apiKey: apiKey,
		model:  model,
		http:   &http.Client{Timeout: 120 * time.Second},
	}
}

type anthropicReq struct {
	Model     string         `json:"model"`
	MaxTokens int            `json:"max_tokens"`
	System    string         `json:"system,omitempty"`
	Messages  []anthropicMsg `json:"messages"`
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
	body, err := json.Marshal(anthropicReq{
		Model:     a.model,
		MaxTokens: maxTokens,
		System:    system,
		Messages:  []anthropicMsg{{Role: "user", Content: user}},
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", a.apiKey)
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
func (a *Anthropic) Questions(ctx context.Context, brief string) ([]string, error) {
	out, err := a.complete(ctx, IntakeSystemPrompt, brief, 500)
	if err != nil {
		return nil, err
	}
	start := strings.Index(out, "[")
	end := strings.LastIndex(out, "]")
	if start < 0 || end < start {
		return nil, nil // no questions → proceed straight to planning
	}
	var qs []string
	if err := json.Unmarshal([]byte(out[start:end+1]), &qs); err != nil {
		return nil, fmt.Errorf("intake: bad JSON: %w", err)
	}
	// Cap defensively.
	if len(qs) > 3 {
		qs = qs[:3]
	}
	return qs, nil
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
