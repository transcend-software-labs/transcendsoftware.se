# Forge design variety — prompt surgery at the choice points

Date: 2026-07-19
Status: approved by user (brainstorming session, approach A "tight prompt surgery")

## Context

Forge (product/, hosted at forge.transcendsoftware.se) generates client sites on
demand. The visual look of a generated site is decided across five stages:

1. **Intake** (`IntakeSystemPrompt`, internal/llm/llm.go) — model suggests 2–3
   design options (palette hexes, fonts, hero layout, image style, signature,
   boldness); the customer picks one. **Palette and type are chosen here.**
2. **Concepts** (`ConceptSystemPrompt`) — the chosen direction becomes exactly
   two hero mockups; the customer picks one.
3. **Planner** (`PlannerSystemPrompt`) — build spec with a `## Design` section.
   All anti-sameness warnings currently live here, one stage *after* the choice.
4. **Build agent** (`BuildSystemPrompt` + template/goapp) — tokens.css /
   components.css (locked) / app.css system, vendored frontend-design skill,
   audit.js + design-review.js gates. Strong; not the problem.
5. **Critic** (`CritiqueSystemPrompt`) — post-deploy vision review, SHIP/POLISH.

The user requirement: generated client projects must look **beautiful and not
alike**. Beauty is already enforced at stages 4–5. Variety leaks at stages 1–3.

## Findings (the three sameness leaks)

1. **Warnings one stage too late.** "Avoid cream+serif+terracotta, near-black+acid,
   broadsheet hairlines" exists only in the planner prompt, but palette/type are
   chosen at intake, where no anti-default guidance exists. The planner is told
   to honor the customer's stated choice.
2. **The intake example IS the overused look.** The prompt's JSON example teaches
   `#F4F0E8 / #171713 / #C64A2E` + `Fraunces / Manrope` — the exact
   cream+terracotta+oldstyle-serif cluster the planner bans. Few-shot examples
   outweigh prose. `FallbackDesignOptions` (ships when the model omits options)
   and the dev-mode `Fake.Concepts` repeat the same cluster.
3. **No diversity constraint between options.** "Make them genuinely different"
   is the only instruction — no axes. Nothing stops a bakery, a law firm and a
   gym all being offered the same warm-serif direction with different adjectives.

## Goal / non-goals

**Goal:** kill systematic sameness at the choice points with small, surgical
prompt edits — no new machinery, no code-path changes, no template changes.

**Non-goals (explicitly out of scope, per user decision):**

- No diversity eval harness / CI gate (rejected approach C).
- No curated font/palette vocabulary reference (rejected approach B).
- No changes to template CSS, the vendored frontend-design skill, DESIGN.md,
  the orchestrator, or parsing code (`openai_compat.go`, `anthropic.go`).
- Existing test fixtures in `intake_test.go` / `retry_test.go` /
  `project/design_test.go` are self-contained parsing fixtures and stay as-is.

## The design

All edits are in `product/internal/llm/llm.go`, plus one new test file.

### 1. `IntakeSystemPrompt` — the main fix

Keep the `questions` job untouched. After the existing `design_options` field
spec, insert a "Deriving the options" block (verbatim):

```
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
```

Replace the JSON shape example's values. The new example is prefixed with a
derivation note — "the example values show the SHAPE for one specific business
(an urban bike workshop); note how every field derives from that business's
world. Derive yours the same way; never copy these values" — and uses:

```json
{"name":"Workshop steel","description":"Industrial warmth drawn from the workshop itself — concrete floor, steel frames, one safety-orange accent","palette":["#EEF0EE","#1B2A33","#C74512","#FFFFFF"],"display_font":"Sora","body_font":"IBM Plex Sans","hero_layout":"split","image_style":"Documentary workshop photography with hard side light, honest grit and a consistent cool-neutral grade; no staged stock smiles, no text overlays","signature":"A frame-geometry dimension drawing crossing the hero (head-tube angle, chainstay length)","boldness":"balanced"}
```

Rationale: light-cool industrial direction, zero overlap with any banned
cluster; `#C74512` passes AA (≈4.9:1) with white button ink; Sora + IBM Plex
Sans are real, distinctive, and cover Cyrillic (sv/ru intakes stay safe).

### 2. `ConceptSystemPrompt` — one guardrail paragraph

Insert before the "Respond with STRICT JSON" line (verbatim):

```
Guardrails: no purple/violet gradients and no cyan-on-dark. Unless the chosen
direction itself is one of these, do not drift the concepts into the three
overused AI defaults (cream + terracotta + oldstyle serif; near-black +
acid-green/vermilion; broadsheet hairlines at zero radius). If the chosen
direction IS one of them — the customer's choice always wins — differentiate
the two concepts through composition and signature, never just color.
```

### 3. `FallbackDesignOptions` — two maximally different worlds

Replace both options (they ship when the intake model omits its array):

- **"Signal & paper"** — light, cool, contemporary. Palette
  `#F5F6F4 / #161A18 / #2457D6 / #FFFFFF` (cobalt accent, AA ≈6.2:1 with white
  ink). Display: "Characterful grotesk (Sora-class)"; body: "Humanist sans
  (Source Sans-class)". Layout split, boldness balanced. ImageStyle: direct
  daylight documentation, cool neutral grade. Signature: one bold graphic crop
  tied to the subject.
- **"Night market"** — warm-dark, bold, NOT near-black+acid. Palette
  `#241D16 / #F4E9D8 / #E3A72F / #33281B` (goldenrod on espresso; dark ink on
  the accent passes AA ≈7.8:1). Display: "Expressive condensed display
  (Oswald-class)"; body: "Clean contemporary sans (PT Sans-class)". Layout
  immersive, boldness bold. ImageStyle: warm tungsten-lit evening series, deep
  shadows, one amber grade. Signature: a full-bleed duotone photo band cut by
  oversized condensed type.

The two differ on all five axes (light/dark, cool/warm, font genre, accent hue
family, temper).

### 4. `Fake.Concepts` (dev mode) — same treatment

Keep the copy/signatures; swap palette + fonts to the two fallback directions
(concept-a: `#F5F6F4/#161A18/#2457D6/#FFFFFF`, Sora / IBM Plex Sans; concept-b:
`#241D16/#F4E9D8/#E3A72F/#33281B`, Oswald / PT Sans) so dev previews stop
teaching the cream/rust look.

### 5. `PlannerSystemPrompt` — one sentence

At the end of the existing "Steer AWAY from the AI-generated tells" paragraph,
append:

```
If the customer's chosen direction itself matches one of these clusters, their
choice wins — keep their palette and type, but realize it distinctively through
composition, the signature and the image treatment rather than reproducing the
cluster's stock look.
```

### 6. New `product/internal/llm/prompts_test.go` — regression guardrails

- `TestIntakePromptExampleIsNotTheOverusedCluster` — IntakeSystemPrompt must
  not contain `#F4F0E8`, `#C64A2E`, or `"display_font":"Fraunces"`.
- `TestIntakePromptCarriesDiversityGuardrails` — must contain `AT LEAST THREE`,
  `axes`, `acid-green`, `Purple/violet`.
- `TestConceptPromptCarriesAntiDefaultGuardrail` — ConceptSystemPrompt must
  contain `acid-green`.
- `TestFallbackDesignOptionsAreDistinctWorlds` — the two fallbacks differ on
  `Palette[0]` (background), `Palette[2]` (accent) and `DisplayFont`, and
  neither uses the old cream/rust pair `#F2EBDD`/`#B9472D`.

## Verification

- `go vet ./... && go test ./...` in `product/` — all green (existing tests
  use their own inline fixtures and are unaffected).
- Manual read-through of the four edited prompts for coherence (no
  contradictions with the surviving "genuinely different" and "avoid generic"
  lines).

## Risks / notes

- Prompt growth: intake prompt grows by ~250 words; sent once per intake call.
  Negligible cost, accepted.
- Prompt steering is probabilistic, not deterministic — accepted (user chose
  approach A over code enforcement).
- The examples themselves could become a new attractor (approach B's risk);
  mitigated by the explicit "derive yours; never copy these values" line and
  the business-context prefix on the example.
- `.superpowers/` (brainstorm companion artifacts) is not in the root
  .gitignore — flagged to the user.
