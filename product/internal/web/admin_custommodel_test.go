package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/transcend-software-labs/rasmus-ai/internal/config"
)

// TestChosenModel exercises the form glue that lets an operator run any
// opencode-reachable model: a typed custom spec wins over the preset dropdown,
// a preset is used when no custom is given, and an invalid custom (malformed,
// or a provider without a configured key) falls back to "" (the Forge default).
func TestChosenModel(t *testing.T) {
	full := &Server{cfg: config.Config{ZenAPIKey: "zk", ZenBaseURL: "https://zen", AnthropicAPIKey: "sk"}}
	noAnthropic := &Server{cfg: config.Config{ZenAPIKey: "zk", ZenBaseURL: "https://zen"}}

	req := func(preset, custom string) *http.Request {
		f := url.Values{}
		f.Set("planner_profile", preset)
		f.Set("planner_custom", custom)
		r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(f.Encode()))
		r.Header.Set("content-type", "application/x-www-form-urlencoded")
		return r
	}

	if got := full.chosenModel(req("sonnet5", "zen/deepseek-v4-pro#high"), "planner"); got != "custom:zen/deepseek-v4-pro#high" {
		t.Errorf("custom should win over preset, got %q", got)
	}
	if got := full.chosenModel(req("sonnet5", ""), "planner"); got != "sonnet5" {
		t.Errorf("preset should be used when no custom, got %q", got)
	}
	if got := full.chosenModel(req("", "  "), "planner"); got != "" {
		t.Errorf("blank custom + blank preset → default, got %q", got)
	}
	if got := noAnthropic.chosenModel(req("", "anthropic/claude-x#max"), "planner"); got != "" {
		t.Errorf("custom with an unconfigured provider must fall back to default, got %q", got)
	}
	if got := full.chosenModel(req("", "garbage-no-slash"), "planner"); got != "" {
		t.Errorf("malformed custom must fall back to default, got %q", got)
	}
	// A valid custom on a configured provider round-trips.
	if got := full.chosenModel(req("", "anthropic/claude-opus-4-8#max"), "planner"); got != "custom:anthropic/claude-opus-4-8#max" {
		t.Errorf("valid custom should be stored, got %q", got)
	}
}
