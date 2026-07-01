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
)

// OpenAICompat implements Intake, Planner and SafetyGate against any
// OpenAI-compatible /chat/completions endpoint — e.g. Moonshot (Kimi K2).
//
// Kimi K2.7 is a reasoning model: it only accepts temperature 1, spends tokens
// on reasoning (so max_tokens must be generous), and returns the answer in
// .content (chain-of-thought goes to .reasoning_content, which we ignore).
type OpenAICompat struct {
	baseURL     string
	apiKey      string
	model       string
	temperature float64
	http        *http.Client
}

// NewOpenAICompat returns a client for an OpenAI-compatible endpoint.
func NewOpenAICompat(baseURL, apiKey, model string) *OpenAICompat {
	return &OpenAICompat{
		baseURL:     strings.TrimRight(baseURL, "/"),
		apiKey:      apiKey,
		model:       model,
		temperature: 1, // required by Kimi reasoning models
		http:        &http.Client{Timeout: 180 * time.Second},
	}
}

type ocMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ocRequest struct {
	Model       string      `json:"model"`
	Messages    []ocMessage `json:"messages"`
	MaxTokens   int         `json:"max_tokens"`
	Temperature float64     `json:"temperature"`
}

type ocResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

func (o *OpenAICompat) complete(ctx context.Context, system, user string, maxTokens int) (string, error) {
	body, err := json.Marshal(ocRequest{
		Model:       o.model,
		MaxTokens:   maxTokens,
		Temperature: o.temperature,
		Messages: []ocMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer "+o.apiKey)

	resp, err := o.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var parsed ocResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("llm: decode response (status %d): %w", resp.StatusCode, err)
	}
	if parsed.Error != nil {
		return "", fmt.Errorf("llm: %s: %s", parsed.Error.Type, parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("llm: no choices (status %d)", resp.StatusCode)
	}
	return parsed.Choices[0].Message.Content, nil
}

// Questions implements Intake.
func (o *OpenAICompat) Questions(ctx context.Context, brief string) ([]string, error) {
	out, err := o.complete(ctx, IntakeSystemPrompt, brief, 3000)
	if err != nil {
		return nil, err
	}
	start := strings.Index(out, "[")
	end := strings.LastIndex(out, "]")
	if start < 0 || end < start {
		return nil, nil
	}
	var qs []string
	if err := json.Unmarshal([]byte(out[start:end+1]), &qs); err != nil {
		return nil, fmt.Errorf("intake: bad JSON: %w", err)
	}
	if len(qs) > 3 {
		qs = qs[:3]
	}
	return qs, nil
}

// Plan implements Planner.
func (o *OpenAICompat) Plan(ctx context.Context, brief string) (PlanResult, error) {
	out, err := o.complete(ctx, PlannerSystemPrompt, brief, 8000)
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
func (o *OpenAICompat) Screen(ctx context.Context, brief, plan string) (GateResult, error) {
	user := "Request:\n" + brief
	if plan != "" {
		user += "\n\nProposed plan:\n" + plan
	}
	out, err := o.complete(ctx, SafetySystemPrompt, user, 3000)
	if err != nil {
		return GateResult{}, err
	}
	return parseVerdict(out)
}
