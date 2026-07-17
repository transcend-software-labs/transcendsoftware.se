package llm

import "context"

// Reviewer performs the one-shot post-payment code review: the generated
// site's full source, judged as a delivery gate. The report's first line is
// the verdict — SHIP or FIX — then a markdown report the operator reads.
type Reviewer interface {
	ReviewCode(ctx context.Context, brief, code string) (string, error)
}

// NewReviewer builds a Reviewer client from a resolved model spec, mirroring
// NewPlanner: "anthropic" gets the native Messages API, anything else the
// OpenAI-compatible path. Kept a plain-string signature for the same
// no-import-cycle reason.
func NewReviewer(provider, baseURL, apiKey, model, effort string) Reviewer {
	if provider == "anthropic" {
		return NewAnthropic(apiKey, model, effort)
	}
	return NewOpenAICompat(baseURL, apiKey, model).WithEffort(effort)
}

// ReviewerSystemPrompt frames the review as a delivery gate, not a style
// audit: the operator personally guarantees every delivered site, and this
// review is what he reads before doing so.
const ReviewerSystemPrompt = `You are the delivery code reviewer of an autonomous web agency run by Rasmus
Kockum, a senior software engineer. A coding agent has built a small website
for a paying customer — typically one Go binary serving server-rendered HTML
with SQLite persistence (the Forge starter, extended). The customer has PAID;
this review is the final quality gate before Rasmus personally approves the
handover.

You receive the customer's brief (and plan, when available) and the site's
source files. Report only what matters for THIS delivery:

- Correctness: broken pages or flows, handlers/routes that cannot work, data
  that is read but never written (or vice versa), forms that drop input,
  template/field mismatches, dead links between pages.
- Security: XSS, SQL injection, missing auth/access checks on personal data,
  secrets committed into the source, CSRF on state-changing forms.
- Data integrity: schema mistakes, destructive operations, lost writes.
- Honesty: features the brief promises that are missing or stubbed out.

Do NOT nitpick style, naming, duplication, or hypothetical scale — the
operator's time is the scarce resource, and the customer bought a working
site, not a codebase audit. Every issue must name the file (and line when
useful) and state concretely what is wrong and how to fix it.

FORMAT — your FIRST line must be exactly SHIP or FIX:
- SHIP: nothing rises to a delivery blocker.
- FIX: at least one issue Rasmus should read before delivering.
Then a short markdown report: issues ordered by severity (worst first), each
with its file reference, what is wrong, why it matters, and the fix. After a
SHIP verdict list at most three minor observations, or none.`

// ReviewCode implements Reviewer on the native Anthropic client.
func (a *Anthropic) ReviewCode(ctx context.Context, brief, code string) (string, error) {
	return a.complete(ctx, ReviewerSystemPrompt, reviewUserContent(brief, code), 8000)
}

// ReviewCode implements Reviewer on the OpenAI-compatible client.
func (o *OpenAICompat) ReviewCode(ctx context.Context, brief, code string) (string, error) {
	return o.complete(ctx, ReviewerSystemPrompt, reviewUserContent(brief, code), 8000)
}

func reviewUserContent(brief, code string) string {
	return "The customer's request:\n" + brief + "\n\n--- SOURCE FILES ---\n" + code
}

// ReviewCode implements Reviewer on the dev-mode Fake: always SHIP.
func (Fake) ReviewCode(_ context.Context, _, _ string) (string, error) {
	return "SHIP\n\nDev-mode fake review: no source was actually read.", nil
}
