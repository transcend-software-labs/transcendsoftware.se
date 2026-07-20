# Forge Design Variety (Prompt Surgery) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop Forge-generated client sites from converging on the same AI-default look by fixing the three sameness leaks at the pipeline's choice points.

**Architecture:** Five surgical edits to prompt constants and fallback values in `product/internal/llm/llm.go` (intake, concepts, planner prompts + `FallbackDesignOptions` + dev-mode `Fake.Concepts`), guarded by a new `prompts_test.go`. No other files change. Spec: `docs/superpowers/specs/2026-07-19-forge-design-variety-design.md`.

**Tech Stack:** Go, stdlib testing. Prompt constants are consumed by `anthropic.go` and `openai_compat.go` (unchanged).

**Git:** commits only with the user's explicit approval (pending — asked once, deferred). All other steps run unconditionally.

---

### Task 1: Failing guardrail tests

**Files:**
- Create: `product/internal/llm/prompts_test.go`

- [ ] **Step 1: Write the failing tests**

```go
package llm

import (
	"strings"
	"testing"
)

// The intake prompt's JSON example once taught the exact cream/rust/serif
// cluster the pipeline warns against — few-shot examples outweigh prose, so
// guard against reintroducing those values.
func TestIntakePromptExampleIsNotTheOverusedCluster(t *testing.T) {
	for _, banned := range []string{"#F4F0E8", "#C64A2E", `"display_font":"Fraunces"`} {
		if strings.Contains(IntakeSystemPrompt, banned) {
			t.Errorf("IntakeSystemPrompt reintroduces overused cluster value %q", banned)
		}
	}
}

// The look is chosen at intake, so the diversity guardrails must live in the
// intake prompt (not only in the later planner prompt).
func TestIntakePromptCarriesDiversityGuardrails(t *testing.T) {
	for _, want := range []string{"AT LEAST THREE", "axes", "acid-green", "Purple/violet"} {
		if !strings.Contains(IntakeSystemPrompt, want) {
			t.Errorf("IntakeSystemPrompt missing diversity guardrail %q", want)
		}
	}
}

func TestConceptPromptCarriesAntiDefaultGuardrail(t *testing.T) {
	if !strings.Contains(ConceptSystemPrompt, "acid-green") {
		t.Error("ConceptSystemPrompt missing the anti-default guardrail")
	}
}

// FallbackDesignOptions ship to customers when the intake model omits its
// design array — they must not collapse into one look, nor into the cluster.
func TestFallbackDesignOptionsAreDistinctWorlds(t *testing.T) {
	opts := FallbackDesignOptions()
	if len(opts) != 2 {
		t.Fatalf("expected 2 fallback options, got %+v", opts)
	}
	a, b := opts[0], opts[1]
	if a.Palette[0] == b.Palette[0] || a.Palette[2] == b.Palette[2] {
		t.Errorf("fallback palettes must differ on background and accent: %+v vs %+v", a.Palette, b.Palette)
	}
	if a.DisplayFont == b.DisplayFont {
		t.Errorf("fallback display fonts must differ, both are %q", a.DisplayFont)
	}
	for _, banned := range []string{"#F2EBDD", "#B9472D"} {
		for _, o := range opts {
			for _, hex := range o.Palette {
				if strings.EqualFold(hex, banned) {
					t.Errorf("fallback %q reintroduces overused cluster value %q", o.Name, banned)
				}
			}
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd product && go test ./internal/llm/ -run 'TestIntakePrompt|TestConceptPrompt|TestFallbackDesignOptions' -v`
Expected: FAIL — all four tests fail against the old prompts (`#F4F0E8` present, no `AT LEAST THREE`, no `acid-green` in concept prompt, `#B9472D` in fallback palette).

---

### Task 2: IntakeSystemPrompt — deriving block + non-cluster example

**Files:**
- Modify: `product/internal/llm/llm.go` (IntakeSystemPrompt constant, ~lines 278–282)

- [ ] **Step 1: Replace the tail of the constant**

Replace exactly this text:

```
   Always provide these fields for every option.

Write questions and design options in the customer's language.
Respond with STRICT JSON and nothing else, exactly this shape:
{"questions":["..."],"design_options":[{"name":"...","description":"...","palette":["#F4F0E8","#171713","#C64A2E","#FFFFFF"],"display_font":"Fraunces","body_font":"Manrope","hero_layout":"asymmetric","image_style":"Directional natural-light photography with tactile close crops and warm restrained grain","signature":"A cropped circular product window crossing the hero grid","boldness":"bold"}]}`
```

with:

```
   Always provide these fields for every option.

Deriving the options (this is what keeps generated sites from looking alike):
- Ground each option in THIS business's own world — its materials, era,
  neighborhood, clientele, product colors — and name the specific you derived
  it from in the description. A direction that would fit any business fits
  none.
- The options must be different worlds, not one look in three colorways:
  they must differ on AT LEAST THREE of these axes — light vs dark
  background; warm vs cool palette; display-font genre (oldstyle serif /
  slab / geometric or grotesk sans / expressive display); accent hue family;
  era & temper (heritage, contemporary, playful, utilitarian, …).
- Never offer these three overused AI-default looks unless the customer
  explicitly asked for one: (1) cream background + terracotta/rust accent +
  oldstyle serif display (Fraunces, Cormorant); (2) near-black background +
  acid-green or vermilion accent; (3) broadsheet hairline-rules layout at
  zero radius. Purple/violet gradients and cyan-on-dark are banned outright.
- Pick display/body faces that actually cover the site's script — many
  display faces are Latin-only, and this agency ships Swedish and Russian
  sites. When unsure, choose faces with documented Cyrillic support.

Write questions and design options in the customer's language.
Respond with STRICT JSON and nothing else, exactly this shape. The example
values show the SHAPE for one specific business (an urban bike workshop) —
note how every field derives from that business's world. Derive yours the
same way; never copy these values:
{"questions":["..."],"design_options":[{"name":"Workshop steel","description":"Industrial warmth drawn from the workshop itself — concrete floor, steel frames, one safety-orange accent","palette":["#EEF0EE","#1B2A33","#C74512","#FFFFFF"],"display_font":"Sora","body_font":"IBM Plex Sans","hero_layout":"split","image_style":"Documentary workshop photography with hard side light, honest grit and a consistent cool-neutral grade; no staged stock smiles, no text overlays","signature":"A frame-geometry dimension drawing crossing the hero (head-tube angle, chainstay length)","boldness":"balanced"}]}`
```

- [ ] **Step 2: Run the intake tests**

Run: `cd product && go test ./internal/llm/ -run 'TestIntakePrompt' -v`
Expected: PASS (both intake tests).

---

### Task 3: ConceptSystemPrompt — guardrail paragraph

**Files:**
- Modify: `product/internal/llm/llm.go` (ConceptSystemPrompt constant, ~lines 310–314)

- [ ] **Step 1: Insert the guardrail paragraph**

Replace exactly:

```
Avoid generic SaaS gradients, floating cards, meaningless stats, centred
everything and two concepts that are merely color swaps. Text must be truthful
to the supplied brief.

Respond with STRICT JSON only:
```

with:

```
Avoid generic SaaS gradients, floating cards, meaningless stats, centred
everything and two concepts that are merely color swaps. Text must be truthful
to the supplied brief.

Guardrails: no purple/violet gradients and no cyan-on-dark. Unless the chosen
direction itself is one of these, do not drift the concepts into the three
overused AI defaults (cream + terracotta + oldstyle serif; near-black +
acid-green/vermilion; broadsheet hairlines at zero radius). If the chosen
direction IS one of them — the customer's choice always wins — differentiate
the two concepts through composition and signature, never just color.

Respond with STRICT JSON only:
```

- [ ] **Step 2: Run the concept test**

Run: `cd product && go test ./internal/llm/ -run 'TestConceptPrompt' -v`
Expected: PASS.

---

### Task 4: FallbackDesignOptions + Fake.Concepts — two distinct worlds

**Files:**
- Modify: `product/internal/llm/llm.go` (FallbackDesignOptions, ~lines 533–536; Fake.Concepts, ~lines 553–554)

- [ ] **Step 1: Replace both fallback options**

Replace exactly:

```go
	return []project.DesignOption{
		{Name: "Clear & distinctive", Description: "Confident hierarchy, crisp spacing and one memorable visual move.", Palette: []string{"#F7F8F5", "#151713", "#2457D6", "#FFFFFF"}, DisplayFont: "Characterful grotesk", BodyFont: "Humanist sans", HeroLayout: "split", ImageStyle: "Honest directional daylight photography with precise, business-specific framing", Signature: "One bold graphic crop tied to the subject", Boldness: "balanced"},
		{Name: "Editorial & expressive", Description: "Stronger type, asymmetric rhythm and tactile image treatment.", Palette: []string{"#F2EBDD", "#241C16", "#B9472D", "#FFF9EE"}, DisplayFont: "Expressive editorial serif", BodyFont: "Clean contemporary sans", HeroLayout: "editorial", ImageStyle: "A cohesive tactile documentary series with natural light and restrained grain", Signature: "An oversized typographic gesture intersecting the image grid", Boldness: "bold"},
	}
```

with:

```go
	return []project.DesignOption{
		{Name: "Signal & paper", Description: "Light, cool and contemporary: crisp grotesk headings, quiet spacing, one cobalt signal color.", Palette: []string{"#F5F6F4", "#161A18", "#2457D6", "#FFFFFF"}, DisplayFont: "Characterful grotesk (Sora-class)", BodyFont: "Humanist sans (Source Sans-class)", HeroLayout: "split", ImageStyle: "Direct daylight documentation with a cool neutral grade and precise, business-specific framing", Signature: "One bold graphic crop tied to the subject", Boldness: "balanced"},
		{Name: "Night market", Description: "Warm-dark and bold: condensed uppercase display, tungsten-warm imagery, goldenrod on deep espresso.", Palette: []string{"#241D16", "#F4E9D8", "#E3A72F", "#33281B"}, DisplayFont: "Expressive condensed display (Oswald-class)", BodyFont: "Clean contemporary sans (PT Sans-class)", HeroLayout: "immersive", ImageStyle: "A warm tungsten-lit evening series with deep shadows and one consistent amber grade", Signature: "A full-bleed duotone photo band cut by oversized condensed type", Boldness: "bold"},
	}
```

- [ ] **Step 2: Update Fake.Concepts palettes/fonts (dev previews stop teaching the cluster)**

Replace the concept-a fragment (unique via `CTA: "Get started"`):

```
CTA: "Get started", Layout: "split", Palette: []string{"#F2EBDD", "#241C16", "#B9472D", "#FFF9EE"}, DisplayFont: "Fraunces", BodyFont: "Manrope", ImageDirection: "A coherent natural-light documentary series with tactile close crops
```

with:

```
CTA: "Get started", Layout: "split", Palette: []string{"#F5F6F4", "#161A18", "#2457D6", "#FFFFFF"}, DisplayFont: "Sora", BodyFont: "IBM Plex Sans", ImageDirection: "A coherent natural-light documentary series with tactile close crops
```

Replace the concept-b fragment (unique via `CTA: "See the offer"`):

```
CTA: "See the offer", Layout: "editorial", Palette: []string{"#F2EBDD", "#241C16", "#B9472D", "#FFF9EE"}, DisplayFont: "Fraunces", BodyFont: "Manrope", ImageDirection: "A coherent natural-light documentary series with wider environmental frames
```

with:

```
CTA: "See the offer", Layout: "editorial", Palette: []string{"#241D16", "#F4E9D8", "#E3A72F", "#33281B"}, DisplayFont: "Oswald", BodyFont: "PT Sans", ImageDirection: "A coherent natural-light documentary series with wider environmental frames
```

- [ ] **Step 3: Run the fallback test**

Run: `cd product && go test ./internal/llm/ -run 'TestFallbackDesignOptions' -v`
Expected: PASS.

---

### Task 5: PlannerSystemPrompt — honor-but-differentiate sentence

**Files:**
- Modify: `product/internal/llm/llm.go` (PlannerSystemPrompt constant, ~lines 191–192)

- [ ] **Step 1: Append the sentence**

Replace exactly:

```
   skill to EXECUTE the look well; your job is to hand it a distinctive,
   specific direction worth executing.
```

with:

```
   skill to EXECUTE the look well; your job is to hand it a distinctive,
   specific direction worth executing. If the customer's chosen direction
   itself matches one of these clusters, their choice wins — keep their
   palette and type, but realize it distinctively through composition, the
   signature and the image treatment rather than reproducing the cluster's
   stock look.
```

- [ ] **Step 2: Package still compiles**

Run: `cd product && go build ./internal/llm/`
Expected: no output (success).

---

### Task 6: Full verification

- [ ] **Step 1: vet + full test suite**

Run: `cd product && go vet ./... && go test ./...`
Expected: all packages PASS (existing tests use their own inline fixtures and are unaffected).

- [ ] **Step 2: Read the four edited prompts end-to-end**

Run: `cd product && sed -n '101,246p;248,330p;529,556p' internal/llm/llm.go` (or read the ranges)
Expected: no contradictions; the surviving "genuinely different" / "avoid generic" lines coexist with the new guardrails.

- [ ] **Step 3: Commit (ONLY with the user's explicit approval)**

```bash
git add product/internal/llm/llm.go product/internal/llm/prompts_test.go docs/superpowers/specs/2026-07-19-forge-design-variety-design.md docs/superpowers/plans/2026-07-19-forge-design-variety.md
git commit -m "llm: move design-diversity guardrails to the intake/concepts choice points"
```
