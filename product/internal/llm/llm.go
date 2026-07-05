// Package llm provides the two model-backed steps of the pipeline:
//
//   - Planner turns a customer brief into a concrete build plan.
//   - SafetyGate screens the request and returns allow/reject/escalate.
//
// Both are interfaces so the orchestrator can run against a deterministic
// Fake (dev mode) or the real Anthropic client (when an API key is set).
//
// The SafetyGate call is deliberately tool-less: it has no capabilities, so a
// jailbreak of it yields at most a bad verdict, never an action.
package llm

import (
	"context"
	"strings"

	"github.com/transcend-software-labs/rasmus-ai/internal/project"
)

// PlanResult is the output of the planning step.
type PlanResult struct {
	Name string // a short human name for the project, derived from the brief
	Plan string // the build plan, in markdown
}

// GateResult is the output of the safety gate.
type GateResult struct {
	Verdict project.Verdict
	Reason  string
}

// Intake produces a short list of PO-level clarifying questions for a brief,
// asked before any planning or building happens.
type Intake interface {
	Questions(ctx context.Context, brief string) ([]string, error)
}

// Planner turns a brief into a build plan.
type Planner interface {
	Plan(ctx context.Context, brief string) (PlanResult, error)
}

// SafetyGate screens a request for abuse/illegality and returns a verdict.
type SafetyGate interface {
	Screen(ctx context.Context, brief, plan string) (GateResult, error)
}

// PlannerSystemPrompt encodes "Rasmus's decisions" — the opinionated default
// taste and stack the agent builds with. This is the product's brain; edit it
// to change what every project defaults to.
const PlannerSystemPrompt = `You are the planning brain of an autonomous web agency run by Rasmus Kockum,
a senior software engineer. A non-technical customer describes a website they
want. Produce a concrete, opinionated BUILD PLAN a coding agent can execute.

Decisions to default to (override only with a clear reason):
- Static or lightly-dynamic marketing sites; no app-style auth unless asked.
- Clean, fast, accessible. EU data residency by default.
- Collect the real content/assets the customer must provide (photos, copy, logo).

Return markdown with these sections:
## Summary        — one paragraph of what we will build
## Pages          — the pages/sections and their purpose
## Stack          — the concrete tech choices
## Data & assets  — what the customer must provide (esp. real photos)
## Open questions — anything that must be clarified before/at build time

Begin the response with a single line: "NAME: <a short 2-4 word project name>".`

// IntakeSystemPrompt drives the clarifying-questions step. The questions are
// what separate this from a tool that confidently builds the wrong thing.
const IntakeSystemPrompt = `You are the intake step of an autonomous web agency. A non-technical customer
has described a website they want. Ask the few highest-value questions a product
owner must answer before building — the ones that would most change the result
if you guessed wrong (e.g. brochure vs. online ordering, who provides photos,
languages, key pages).

Ask at most 3 questions. Be concrete and in plain language; no jargon.
Respond with STRICT JSON and nothing else: a JSON array of question strings,
e.g. ["Do you want customers to buy online, or just contact you?", "..."].
If the brief is already complete, return [].`

// BuildSystemPrompt drives the build agent (opencode) inside the sandbox: build
// the site from the plan, then deploy it. The FLY_APP/FLY_DEPLOY_TOKEN env vars
// are set in the sandbox; the shell expands them.
const BuildSystemPrompt = `You are an autonomous build agent for Rasmus Kockum's web agency. Build the
website described below in the current working directory (/workspace).

How to build:
- Static site by default: plain, valid HTML/CSS. Fast, accessible, Swedish unless
  told otherwise. Write real, complete files — never just describe them.
- Use the customer's uploaded files in /workspace/assets/ if present; copy the
  ones you use into the site. Only use placeholders if assets/ is empty.

Then make it deployable and deploy it:
- Create a Dockerfile that serves the site over HTTP on port 8080
  (e.g. FROM nginx:alpine, copy files to /usr/share/nginx/html, make nginx
  listen on 8080), and a fly.toml with primary_region "arn" and an
  [http_service] section with exactly: internal_port = 8080,
  force_https = true, auto_stop_machines = "stop", auto_start_machines = true,
  min_machines_running = 0. The auto-stop settings are required — previews
  must cost nothing while nobody is looking at them.
- Deploy by running exactly this command:
  fly deploy --remote-only --app "$FLY_APP" --access-token "$FLY_DEPLOY_TOKEN"
- Confirm the deploy finished successfully.

Build this:`

// SafetySystemPrompt instructs the gate to return strict JSON only.
const SafetySystemPrompt = `You are a safety screen for an autonomous website-building service. Given a
customer's request, decide whether it is acceptable to build.

Reject requests that are clearly for: phishing or credential harvesting, scams
or fraud, malware or attack tooling, illegal goods/services, impersonation of a
real person or brand, sexual content involving minors, or harassment.

Escalate (do not auto-decide) requests that are ambiguous, plausibly regulated
(medical, financial, gambling, legal advice), involve collecting sensitive
personal/health data, or that you are unsure about.

Allow ordinary legitimate websites (businesses, portfolios, blogs, events).

Respond with STRICT JSON and nothing else:
{"verdict":"allow|reject|escalate","reason":"<one sentence>"}`

// Fake is a deterministic Planner + SafetyGate for dev mode. It needs no
// network and lets both the allow and reject paths be exercised locally:
// a brief containing an obvious-abuse keyword is rejected.
type Fake struct{}

// NewFake returns a deterministic dev planner/gate.
func NewFake() *Fake { return &Fake{} }

func (Fake) Questions(_ context.Context, _ string) ([]string, error) {
	return []string{
		"Do you want customers to buy online, or just see the site and contact you?",
		"Do you have your own photos and logo, or should we use placeholders for now?",
		"What language(s) should the site be in?",
	}, nil
}

func (Fake) Plan(_ context.Context, brief string) (PlanResult, error) {
	name := deriveName(brief)
	plan := "## Summary\nA website for: " + strings.TrimSpace(brief) + "\n\n" +
		"## Pages\n- Home\n- About\n- Contact\n\n" +
		"## Stack\nStatic site, deployed to Fly, EU region.\n\n" +
		"## Data & assets\n- Real photos\n- Copy / wording\n- Logo (optional)\n\n" +
		"## Open questions\n- Brochure only, or online ordering?\n\n" +
		"_(dev-mode plan — set ANTHROPIC_API_KEY for real planning)_"
	return PlanResult{Name: name, Plan: plan}, nil
}

var abuseKeywords = []string{
	"phishing", "phish", "malware", "ransomware", "carding", "stolen credit",
	"login page for", "clone of", "ddos", "botnet", "keylogger",
}

func (Fake) Screen(_ context.Context, brief, _ string) (GateResult, error) {
	low := strings.ToLower(brief)
	for _, kw := range abuseKeywords {
		if strings.Contains(low, kw) {
			return GateResult{
				Verdict: project.VerdictReject,
				Reason:  "Request matched a disallowed pattern (dev-mode screen).",
			}, nil
		}
	}
	return GateResult{Verdict: project.VerdictAllow, Reason: "Looks like an ordinary website (dev-mode screen)."}, nil
}

func deriveName(brief string) string {
	fields := strings.Fields(brief)
	if len(fields) == 0 {
		return "New project"
	}
	if len(fields) > 4 {
		fields = fields[:4]
	}
	return strings.Title(strings.ToLower(strings.Join(fields, " "))) //nolint:staticcheck // simple dev-mode title
}
