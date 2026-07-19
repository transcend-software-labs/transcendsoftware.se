package llm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/project"
)

// OpenAICompat implements Intake, Planner and SafetyGate against any
// OpenAI-compatible /chat/completions endpoint — e.g. Moonshot (Kimi K2).
//
// Kimi K2.7 is a reasoning model: it only accepts temperature 1, spends tokens
// on reasoning (so max_tokens must be generous), and returns the answer in
// .content (chain-of-thought goes to .reasoning_content, which we ignore).
type OpenAICompat struct {
	baseURL         string
	apiKey          string
	model           string
	protocol        string // "" = chat/completions; "responses" = Responses API
	temperature     float64
	reasoningEffort string // "" = model default
	http            *http.Client
	maxAttempts     int           // total tries per call (1 + retries)
	retryDelay      time.Duration // backoff between attempts
}

// WithEffort sets the reasoning effort (best-effort — ignored by gateways that
// don't support it) and returns the client for chaining.
func (o *OpenAICompat) WithEffort(effort string) *OpenAICompat {
	o.reasoningEffort = effort
	return o
}

// WithProtocol selects a non-chat API exposed by a compatible gateway.
func (o *OpenAICompat) WithProtocol(protocol string) *OpenAICompat {
	o.protocol = protocol
	return o
}

// NewOpenAICompat returns a client for an OpenAI-compatible endpoint.
func NewOpenAICompat(baseURL, apiKey, model string) *OpenAICompat {
	return &OpenAICompat{
		baseURL:     strings.TrimRight(baseURL, "/"),
		apiKey:      apiKey,
		model:       model,
		temperature: 1, // required by Kimi reasoning models
		http:        &http.Client{Timeout: 180 * time.Second},
		maxAttempts: 2, // one retry — Kimi is slow and occasionally blips
		retryDelay:  2 * time.Second,
	}
}

type ocMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ocRequest struct {
	Model    string      `json:"model"`
	Messages []ocMessage `json:"messages"`
	// Exactly one of the token caps is set. OpenAI's own API rejects max_tokens
	// on GPT-5.x reasoning models (it requires max_completion_tokens) and only
	// accepts the default temperature, so temperature is omitted there; every
	// other OpenAI-compatible gateway we use (Zen, Moonshot) takes the classic
	// max_tokens + temperature pair.
	MaxTokens           int      `json:"max_tokens,omitempty"`
	MaxCompletionTokens int      `json:"max_completion_tokens,omitempty"`
	Temperature         *float64 `json:"temperature,omitempty"`
	// ReasoningEffort steers reasoning models (OpenAI-style). Best-effort — a
	// gateway that doesn't support it ignores the field. Empty = model default.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
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

type responsesRequest struct {
	Model           string              `json:"model"`
	Instructions    string              `json:"instructions"`
	Input           string              `json:"input"`
	MaxOutputTokens int                 `json:"max_output_tokens"`
	Reasoning       *responsesReasoning `json:"reasoning,omitempty"`
}

type responsesReasoning struct {
	Effort string `json:"effort"`
}

type responsesResponse struct {
	OutputText string `json:"output_text"`
	Output     []struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"output"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// complete calls the model, retrying transient failures (network blips, 429s,
// 5xx, empty responses) up to maxAttempts. Permanent failures (4xx other than
// 429, auth) are returned immediately — retrying them just wastes time.
func (o *OpenAICompat) complete(ctx context.Context, system, user string, maxTokens int) (string, error) {
	var lastErr error
	for attempt := 1; attempt <= o.maxAttempts; attempt++ {
		content, retryable, err := o.completeOnce(ctx, system, user, maxTokens)
		if err == nil {
			return content, nil
		}
		lastErr = err
		if !retryable || attempt == o.maxAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(o.retryDelay):
		}
	}
	return "", lastErr
}

// completeOnce performs a single request. The bool reports whether the failure
// is worth retrying.
func (o *OpenAICompat) completeOnce(ctx context.Context, system, user string, maxTokens int) (string, bool, error) {
	if o.protocol == "responses" {
		return o.completeResponsesOnce(ctx, system, user, maxTokens)
	}
	r := ocRequest{
		Model: o.model,
		Messages: []ocMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		ReasoningEffort: o.reasoningEffort,
	}
	if strings.Contains(o.baseURL, "api.openai.com") {
		// max_completion_tokens caps hidden reasoning + visible output COMBINED
		// on GPT-5.x. Reasoning-heavy calls (planning) can spend a whole classic
		// budget before writing a single visible token, which comes back as an
		// empty 200. 4x is a ceiling, not a spend — only generated tokens bill.
		r.MaxCompletionTokens = maxTokens * 4
	} else {
		r.MaxTokens = maxTokens
		r.Temperature = &o.temperature
	}
	body, err := json.Marshal(r)
	if err != nil {
		return "", false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", false, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer "+o.apiKey)

	resp, err := o.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", false, ctx.Err() // caller cancelled/timed out — don't retry
		}
		return "", true, fmt.Errorf("llm: request failed: %w", err) // transport blip
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	// 429 (rate limit) and 5xx are transient; other non-2xx are permanent.
	retryableStatus := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500

	var parsed ocResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", retryableStatus, fmt.Errorf("llm: decode response (status %d): %w", resp.StatusCode, err)
	}
	if parsed.Error != nil {
		return "", retryableStatus, fmt.Errorf("llm: %s: %s", parsed.Error.Type, parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 || parsed.Choices[0].Message.Content == "" {
		// Empty at 200 can be a transient hiccup; give it one more try.
		return "", retryableStatus || resp.StatusCode == http.StatusOK,
			fmt.Errorf("llm: empty response (status %d)", resp.StatusCode)
	}
	return parsed.Choices[0].Message.Content, false, nil
}

// completeResponsesOnce uses the OpenAI Responses API shape. OpenCode Zen's
// GPT 5.6 family is only exposed on /responses, not /chat/completions.
func (o *OpenAICompat) completeResponsesOnce(ctx context.Context, system, user string, maxTokens int) (string, bool, error) {
	r := responsesRequest{
		Model: o.model, Instructions: system, Input: user,
		// The cap includes hidden reasoning. Keep the existing GPT headroom so a
		// deep planning call does not finish before emitting visible text.
		MaxOutputTokens: maxTokens * 4,
	}
	if o.reasoningEffort != "" {
		r.Reasoning = &responsesReasoning{Effort: o.reasoningEffort}
	}
	body, err := json.Marshal(r)
	if err != nil {
		return "", false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/responses", bytes.NewReader(body))
	if err != nil {
		return "", false, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer "+o.apiKey)

	resp, err := o.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", false, ctx.Err()
		}
		return "", true, fmt.Errorf("llm: request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	retryableStatus := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500

	var parsed responsesResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", retryableStatus, fmt.Errorf("llm: decode response (status %d): %w", resp.StatusCode, err)
	}
	if parsed.Error != nil {
		return "", retryableStatus, fmt.Errorf("llm: %s: %s", parsed.Error.Type, parsed.Error.Message)
	}
	content := parsed.OutputText
	if content == "" {
		var sb strings.Builder
		for _, item := range parsed.Output {
			for _, part := range item.Content {
				if part.Type == "output_text" {
					sb.WriteString(part.Text)
				}
			}
		}
		content = sb.String()
	}
	if content == "" {
		return "", retryableStatus || resp.StatusCode == http.StatusOK,
			fmt.Errorf("llm: empty response (status %d)", resp.StatusCode)
	}
	return content, false, nil
}

// Questions implements Intake.
func (o *OpenAICompat) Questions(ctx context.Context, brief, lang string) (IntakeResult, error) {
	out, err := o.complete(ctx, IntakeSystemPrompt+intakeLangDirective(lang), brief, 3000)
	if err != nil {
		return IntakeResult{}, err
	}
	return parseIntakeJSON(out)
}

// Concepts implements the second, visual intake gate. It deliberately uses
// structured JSON instead of accepting model-written HTML/CSS into Forge.
func (o *OpenAICompat) Concepts(ctx context.Context, brief, design, lang string) (ConceptResult, error) {
	user := "Customer brief:\n" + brief + "\n\nChosen design direction:\n" + design
	out, err := o.complete(ctx, ConceptSystemPrompt+conceptLangDirective(lang), user, 4000)
	if err != nil {
		return ConceptResult{}, err
	}
	return parseConceptJSON(out)
}

// parseIntakeJSON extracts the intake JSON object (questions + design options)
// from a model response, capping both defensively. Shared by all LLM clients.
func parseIntakeJSON(out string) (IntakeResult, error) {
	start := strings.Index(out, "{")
	end := strings.LastIndex(out, "}")
	if start < 0 || end < start {
		return IntakeResult{}, nil
	}
	var parsed struct {
		Questions     []string               `json:"questions"`
		DesignOptions []project.DesignOption `json:"design_options"`
	}
	if err := json.Unmarshal([]byte(out[start:end+1]), &parsed); err != nil {
		return IntakeResult{}, fmt.Errorf("intake: bad JSON: %w", err)
	}
	res := IntakeResult{Questions: parsed.Questions}
	if len(res.Questions) > 3 {
		res.Questions = res.Questions[:3]
	}
	for i, d := range parsed.DesignOptions {
		if i == 3 || d.Name == "" {
			break
		}
		d.Name = cleanConceptText(d.Name, 80)
		d.Description = cleanConceptText(d.Description, 300)
		d.Palette = cleanPalette(d.Palette)
		d.DisplayFont = cleanConceptText(d.DisplayFont, 80)
		d.BodyFont = cleanConceptText(d.BodyFont, 80)
		d.HeroLayout = cleanEnum(d.HeroLayout, []string{"split", "editorial", "immersive", "framed", "asymmetric"}, "split")
		d.ImageStyle = cleanConceptText(d.ImageStyle, 400)
		d.Signature = cleanConceptText(d.Signature, 300)
		d.Boldness = cleanEnum(d.Boldness, []string{"restrained", "balanced", "bold"}, "balanced")
		d.HeroConcepts = nil // intake cannot smuggle a pre-selected concept
		res.DesignOptions = append(res.DesignOptions, d)
	}
	return res, nil
}

func parseConceptJSON(out string) (ConceptResult, error) {
	start := strings.Index(out, "{")
	end := strings.LastIndex(out, "}")
	if start < 0 || end < start {
		return ConceptResult{}, fmt.Errorf("concepts: missing JSON")
	}
	var parsed ConceptResult
	if err := json.Unmarshal([]byte(out[start:end+1]), &parsed); err != nil {
		return ConceptResult{}, fmt.Errorf("concepts: bad JSON: %w", err)
	}
	if len(parsed.Concepts) < 2 {
		return ConceptResult{}, fmt.Errorf("concepts: wanted two concepts, got %d", len(parsed.Concepts))
	}
	parsed.Concepts = parsed.Concepts[:2]
	for i := range parsed.Concepts {
		c := &parsed.Concepts[i]
		c.ID = []string{"concept-a", "concept-b"}[i]
		c.Name = cleanConceptText(c.Name, 80)
		c.Rationale = cleanConceptText(c.Rationale, 300)
		c.Eyebrow = cleanConceptText(c.Eyebrow, 100)
		c.Headline = cleanConceptText(c.Headline, 160)
		c.Subhead = cleanConceptText(c.Subhead, 320)
		c.CTA = cleanConceptText(c.CTA, 80)
		c.Layout = cleanEnum(c.Layout, []string{"split", "editorial", "immersive", "framed", "asymmetric"}, []string{"split", "editorial"}[i])
		c.Palette = cleanPalette(c.Palette)
		c.DisplayFont = cleanConceptText(c.DisplayFont, 80)
		c.BodyFont = cleanConceptText(c.BodyFont, 80)
		c.ImageDirection = cleanConceptText(c.ImageDirection, 600)
		c.Signature = cleanConceptText(c.Signature, 300)
		c.Selected = false
		if c.Name == "" || c.Headline == "" || c.CTA == "" || c.ImageDirection == "" {
			return ConceptResult{}, fmt.Errorf("concepts: concept %d is incomplete", i+1)
		}
	}
	if parsed.Concepts[0].Layout == parsed.Concepts[1].Layout {
		if parsed.Concepts[0].Layout == "editorial" {
			parsed.Concepts[1].Layout = "split"
		} else {
			parsed.Concepts[1].Layout = "editorial"
		}
	}
	return parsed, nil
}

func cleanConceptText(s string, max int) string {
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	runes := []rune(s)
	if len(runes) > max {
		s = string(runes[:max])
	}
	return s
}

func cleanEnum(value string, allowed []string, fallback string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	for _, candidate := range allowed {
		if value == candidate {
			return value
		}
	}
	return fallback
}

func cleanPalette(values []string) []string {
	out := make([]string, 0, 5)
	for _, value := range values {
		value = strings.ToUpper(strings.TrimSpace(value))
		if len(value) != 7 || value[0] != '#' {
			continue
		}
		valid := true
		for _, ch := range value[1:] {
			if !strings.ContainsRune("0123456789ABCDEF", ch) {
				valid = false
				break
			}
		}
		if valid {
			out = append(out, value)
		}
		if len(out) == 5 {
			break
		}
	}
	return out
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

// --- Vision: design critique -------------------------------------------------

// mmPart is one element of a multimodal message ("text" or "image_url").
type mmPart struct {
	Type     string      `json:"type"`
	Text     string      `json:"text,omitempty"`
	ImageURL *mmImageURL `json:"image_url,omitempty"`
}

type mmImageURL struct {
	URL string `json:"url"`
}

type mmMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string (system) or []mmPart (user)
}

type mmRequest struct {
	Model               string      `json:"model"`
	Messages            []mmMessage `json:"messages"`
	MaxTokens           int         `json:"max_tokens,omitempty"`
	MaxCompletionTokens int         `json:"max_completion_tokens,omitempty"`
	Temperature         *float64    `json:"temperature,omitempty"`
}

// CritiqueDesign shows the model the deployed site's page screenshots (PNG)
// alongside the plan's design brief and returns its review. Best-effort by
// contract: callers must treat an error as "no critique", never as a build
// failure — not every gateway/model accepts images.
func (o *OpenAICompat) CritiqueDesign(ctx context.Context, brief string, pngs [][]byte) (string, error) {
	parts := []mmPart{{Type: "text", Text: brief}}
	for i, png := range pngs {
		if i == 4 {
			break // enough signal; keeps the request under gateway body limits
		}
		parts = append(parts, mmPart{Type: "image_url", ImageURL: &mmImageURL{
			URL: "data:image/png;base64," + base64.StdEncoding.EncodeToString(png),
		}})
	}
	r := mmRequest{
		Model: o.model,
		Messages: []mmMessage{
			{Role: "system", Content: CritiqueSystemPrompt},
			{Role: "user", Content: parts},
		},
	}
	const maxTokens = 2000
	if strings.Contains(o.baseURL, "api.openai.com") {
		r.MaxCompletionTokens = maxTokens * 4
	} else {
		r.MaxTokens = maxTokens
		r.Temperature = &o.temperature
	}
	body, err := json.Marshal(r)
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
		return "", fmt.Errorf("critic: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var parsed ocResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("critic: decode (status %d): %w", resp.StatusCode, err)
	}
	if parsed.Error != nil {
		return "", fmt.Errorf("critic: %s: %s", parsed.Error.Type, parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 || parsed.Choices[0].Message.Content == "" {
		return "", fmt.Errorf("critic: empty response (status %d)", resp.StatusCode)
	}
	return parsed.Choices[0].Message.Content, nil
}
