# Forge Control Plane UI Refinement Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix five audited UI defects in the Forge control plane (footer collision, foreign palette hues, blob art, artifact-less landing, run-on summaries) plus a proper dashboard empty state — all inside the existing design system.

**Architecture:** One Go function (`previewPaletteVars`), one stylesheet (`app.css`), two templates (`landing.html`, `dashboard.html`), three locale files (one new key each — two total new keys). Spec: `docs/superpowers/specs/2026-07-20-forge-control-plane-ui-design.md`.

**Tech Stack:** Go html/template, plain CSS (tokens from `:root`), Go tests + i18n parity test.

**Git:** commits only with the user's explicit approval.

---

### Task 1: P0-2 — palette derivation in `previewPaletteVars`

**Files:**
- Modify: `product/internal/web/handlers_projects.go:419-433`

- [ ] **Step 1: Replace the fallback and emit derived accent-2**

Replace exactly:

```go
var previewHex = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

func previewPaletteVars(palette []string) string {
	fallback := []string{"#F5F1E8", "#171713", "#D4FF3F", "#FFFFFF", "#8277FF"}
	colors := append([]string(nil), fallback...)
	for i, color := range palette {
		if i == len(colors) {
			break
		}
		if previewHex.MatchString(color) {
			colors[i] = strings.ToUpper(color)
		}
	}
	return fmt.Sprintf("--pv-bg:%s;--pv-ink:%s;--pv-accent:%s;--pv-surface:%s;--pv-accent-2:%s", colors[0], colors[1], colors[2], colors[3], colors[4])
}
```

with:

```go
var previewHex = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// previewPaletteVars renders a tile/concept palette into inline CSS custom
// properties. Missing slots fall back to a neutral, non-cluster set (never the
// AI tells: no cream/terracotta, acid-green, or purple), and a missing second
// accent DERIVES from the palette itself — a foreign hue can never leak into
// the previews the customer judges our taste by.
func previewPaletteVars(palette []string) string {
	fallback := []string{"#F5F6F4", "#161A18", "#2457D6", "#FFFFFF", ""}
	colors := append([]string(nil), fallback...)
	for i, color := range palette {
		if i == len(colors) {
			break
		}
		if previewHex.MatchString(color) {
			colors[i] = strings.ToUpper(color)
		}
	}
	accent2 := colors[4]
	if accent2 == "" {
		accent2 = "color-mix(in srgb, var(--pv-accent) 55%, var(--pv-bg))"
	}
	return fmt.Sprintf("--pv-bg:%s;--pv-ink:%s;--pv-accent:%s;--pv-surface:%s;--pv-accent-2:%s", colors[0], colors[1], colors[2], colors[3], accent2)
}
```

- [ ] **Step 2: Run web tests**

Run: `cd product && go test ./internal/web/ -run TestFullFlow -v`
Expected: PASS (the test asserts only the `--pv-bg:#F5F6F4` prefix, still true).

---

### Task 2: P0-3 — duotone media-frame art

**Files:**
- Modify: `product/internal/web/static/app.css` (`.dt-art` rules ~line 723-726, `.concept-visual` rules ~line 792-795)

- [ ] **Step 1: Replace the `.dt-art` blob fills**

Replace exactly:

```css
.dt-art::before, .dt-art::after, .dt-art i { content: ""; position: absolute; display: block; }
.dt-art::before { inset: 12% 10% 25% 18%; border-radius: 50% 35% 48% 32%; background: var(--pv-accent); }
.dt-art::after { width: 55%; aspect-ratio: 1; right: -12%; bottom: -8%; border-radius: 50%; background: var(--pv-accent-2); opacity: .75; }
.dt-art i { width: 58%; height: .3rem; left: 12%; bottom: 12%; background: var(--pv-ink); opacity: .35; }
```

with:

```css
.dt-art::before, .dt-art::after, .dt-art i { content: ""; position: absolute; display: block; }
/* Duotone media panel + circular subject crop — the signature "commissioned
   photo series" device, built only from the palette's own colors. */
.dt-art::before {
  inset: 10% 10% 22% 12%; border-radius: calc(var(--radius) - 4px);
  background: linear-gradient(150deg,
    color-mix(in srgb, var(--pv-accent) 84%, var(--pv-ink)),
    color-mix(in srgb, var(--pv-accent) 28%, var(--pv-surface)));
  box-shadow: inset 0 0 0 1px color-mix(in srgb, var(--pv-ink) 16%, transparent);
}
.dt-art::after {
  width: 42%; aspect-ratio: 1; right: 5%; bottom: 4%; border-radius: 50%;
  background: var(--pv-surface);
  box-shadow: 0 0 0 .22rem var(--pv-accent-2), 0 .3rem .9rem color-mix(in srgb, var(--pv-ink) 35%, transparent);
}
.dt-art i { width: 58%; height: .3rem; left: 12%; bottom: 12%; background: var(--pv-ink); opacity: .35; }
```

- [ ] **Step 2: Replace the `.concept-visual` blob fills**

Replace exactly:

```css
.concept-visual::before, .concept-visual::after, .concept-visual i { content: ""; position: absolute; display: block; }
.concept-visual::before { inset: 9% 12% 18% 9%; border-radius: 48% 38% 52% 31%; background: var(--pv-accent); }
.concept-visual::after { width: 55%; aspect-ratio: 1; right: -12%; bottom: -8%; border-radius: 50%; background: var(--pv-accent-2); opacity: .72; }
.concept-visual i { width: 63%; height: .45rem; left: 12%; bottom: 11%; background: var(--pv-ink); opacity: .3; }
```

with:

```css
.concept-visual::before, .concept-visual::after, .concept-visual i { content: ""; position: absolute; display: block; }
.concept-visual::before {
  inset: 9% 10% 20% 9%; border-radius: calc(var(--radius) - 4px);
  background: linear-gradient(150deg,
    color-mix(in srgb, var(--pv-accent) 84%, var(--pv-ink)),
    color-mix(in srgb, var(--pv-accent) 28%, var(--pv-surface)));
  box-shadow: inset 0 0 0 1px color-mix(in srgb, var(--pv-ink) 16%, transparent);
}
.concept-visual::after {
  width: 40%; aspect-ratio: 1; right: 6%; bottom: 6%; border-radius: 50%;
  background: var(--pv-surface);
  box-shadow: 0 0 0 .22rem var(--pv-accent-2), 0 .3rem .9rem color-mix(in srgb, var(--pv-ink) 35%, transparent);
}
.concept-visual i { width: 63%; height: .45rem; left: 12%; bottom: 11%; background: var(--pv-ink); opacity: .3; }
```

---

### Task 3: P0-1 footer + P1-5 summaries (app.css)

**Files:**
- Modify: `product/internal/web/static/app.css` (`.foot` line 889, `.brief` line 471)

- [ ] **Step 1: Footer becomes a stacked, separated pair**

Replace exactly:

```css
.foot { border-top: 1px solid var(--border); padding: 1.75rem 1.25rem; text-align: center; font-size: var(--fs-sm); flex-shrink: 0; }
```

with:

```css
.foot { border-top: 1px solid var(--border); padding: 1.75rem 1.25rem; text-align: center; font-size: var(--fs-sm); flex-shrink: 0; display: flex; flex-direction: column; align-items: center; gap: var(--sp-2); }
.foot .lang-switch { border-top: 1px solid var(--border); padding-top: var(--sp-2); display: inline-flex; gap: var(--sp-3); }
```

- [ ] **Step 2: Summary blocks keep their line breaks**

Replace exactly:

```css
.brief { color: var(--muted); }
```

with:

```css
.brief { color: var(--muted); white-space: pre-line; }
```

---

### Task 4: P1-6 — dashboard empty state

**Files:**
- Modify: `product/internal/web/templates/dashboard.html:19-21`
- Modify: `product/internal/web/static/app.css` (`.empty` line 394)
- Modify: `product/internal/web/i18n/locales/en.json`, `sv.json`, `ru.json` (add `dash.empty_sub`)

- [ ] **Step 1: New markup**

Replace exactly:

```html
{{else}}
<p class="muted empty">{{.T "dash.empty"}} <a href="/projects/new">{{.T "dash.empty_link"}}</a></p>
{{end}}
```

with:

```html
{{else}}
<div class="empty-state">
  <p class="empty-title">{{.T "dash.empty"}}</p>
  <p class="muted">{{.T "dash.empty_sub"}}</p>
  <a class="btn" href="/projects/new">{{.T "dash.empty_link"}}</a>
</div>
{{end}}
```

- [ ] **Step 2: CSS — replace the bare `.empty` rule**

Replace exactly:

```css
.empty { margin-top: var(--sp-6); }
```

with:

```css
.empty { margin-top: var(--sp-6); }
.empty-state {
  display: grid; justify-items: center; gap: var(--sp-3); text-align: center;
  padding: var(--sp-7) var(--sp-5); margin-top: var(--sp-6);
  border: 1px dashed var(--border-2); border-radius: var(--radius-lg); background: var(--surface);
}
.empty-state .empty-title { margin: 0; font-size: var(--fs-xl); font-weight: 600; color: var(--text); }
.empty-state p { margin: 0; }
```

- [ ] **Step 3: i18n keys (insert alphabetically after `dash.empty`)**

en.json, after `"dash.empty": "No projects yet.",`:
```json
  "dash.empty_sub": "Describe your business in a few sentences — the first preview is free.",
```

sv.json, after `"dash.empty": "Inga projekt ännu.",`:
```json
  "dash.empty_sub": "Beskriv din verksamhet i några meningar — den första förhandsvisningen är gratis.",
```

ru.json, after `"dash.empty": "Проектов пока нет.",`:
```json
  "dash.empty_sub": "Опишите ваш бизнес в нескольких предложениях — первый предпросмотр бесплатный.",
```

- [ ] **Step 4: i18n parity + web tests**

Run: `cd product && go test ./internal/web/... `
Expected: PASS (parity test covers the new key).

---

### Task 5: P1-4 — landing hero artifact

**Files:**
- Modify: `product/internal/web/templates/landing.html:2-7`
- Modify: `product/internal/web/static/app.css` (`.hero` rules ~line 219-223; mobile block at end)
- Modify: locale files (add `landing.demo.badge`)

- [ ] **Step 1: Template — append the demo inside `.hero`**

Replace exactly:

```html
<section class="hero">
  <h1>{{.T "landing.h1a"}}<br>{{.T "landing.h1b"}}</h1>
  <p class="lead">{{.T "landing.lead1"}}<strong>{{.T "landing.lead_strong"}}</strong>{{.T "landing.lead2"}}</p>
  <a class="btn btn-lg" href="{{.StartURL}}">{{.T "landing.cta"}}</a>
  <p class="muted small">{{with .Data}}{{if .DomainBuy}}{{$.T "landing.sub_domain"}}{{else}}{{$.T "landing.sub"}}{{end}}{{else}}{{.T "landing.sub"}}{{end}}</p>
</section>
```

with:

```html
<section class="hero">
  <h1>{{.T "landing.h1a"}}<br>{{.T "landing.h1b"}}</h1>
  <p class="lead">{{.T "landing.lead1"}}<strong>{{.T "landing.lead_strong"}}</strong>{{.T "landing.lead2"}}</p>
  <a class="btn btn-lg" href="{{.StartURL}}">{{.T "landing.cta"}}</a>
  <p class="muted small">{{with .Data}}{{if .DomainBuy}}{{$.T "landing.sub_domain"}}{{else}}{{$.T "landing.sub"}}{{end}}{{else}}{{.T "landing.sub"}}{{end}}</p>
  <div class="hero-demo">
    <span class="badge badge-accent hero-demo-badge">{{.T "landing.demo.badge"}}</span>
    <span class="concept-device-pair preview-layout-split preview-type-sans" aria-hidden="true">
      <span class="concept-desktop">
        <span class="concept-nav"><b>Söder &amp; Surdeg</b><i></i><i></i></span>
        <span class="concept-hero-copy">
          <small>Bakery · Södermalm</small>
          <strong>Sourdough, baked slow.</strong>
          <span>Stone-oven loaves and cardamom buns. Order tonight, pick up warm tomorrow.</span>
          <em>See the menu</em>
        </span>
        <span class="concept-visual"><i></i></span>
      </span>
      <span class="concept-mobile">
        <span class="concept-nav"><b>Söder &amp; Surdeg</b><i></i></span>
        <span class="concept-hero-copy">
          <small>Bakery · Södermalm</small>
          <strong>Sourdough, baked slow.</strong>
          <span>Stone-oven loaves and cardamom buns.</span>
          <em>See the menu</em>
        </span>
        <span class="concept-visual"><i></i></span>
      </span>
    </span>
  </div>
</section>
```

- [ ] **Step 2: CSS — hero demo rules (insert after the `.hero .small` rule)**

```css
/* Hero artifact: the product's own concept mockup, dogfooded as the demo.
   Palette scoped here (CSP bans inline styles) — the Signal & paper values. */
.hero-demo { margin-top: var(--sp-6); animation: rise .5s .32s var(--ease) both; }
.hero-demo-badge { margin-bottom: var(--sp-2); display: inline-block; }
.hero-demo .concept-device-pair {
  --pv-bg: #F5F6F4; --pv-ink: #161A18; --pv-accent: #2457D6; --pv-surface: #FFFFFF;
  --pv-accent-2: color-mix(in srgb, var(--pv-accent) 55%, var(--pv-bg));
}
.hero-demo .concept-desktop { min-height: 20rem; border-radius: var(--radius); }
.hero-demo .concept-mobile { border-radius: .8rem; }
```

(The existing `.hero-demo`-less concept classes supply everything else; `.concept-device-pair` is `display:block` so spans are fine.)

- [ ] **Step 3: Mobile block — add inside `@media (max-width: 720px)` (with the other concept rules, after `.concept-notes { grid-template-columns: 1fr; }`)**

```css
  .hero-demo .concept-desktop { min-height: 16rem; }
```

- [ ] **Step 4: i18n keys (insert alphabetically in the `landing.*` block)**

en.json (before `"landing.domain.existing"` — keep alphabetical):
```json
  "landing.demo.badge": "Sample preview",
```

sv.json (same relative position):
```json
  "landing.demo.badge": "Exempel på förhandsvisning",
```

ru.json (same relative position):
```json
  "landing.demo.badge": "Пример предпросмотра",
```

- [ ] **Step 5: i18n parity + web tests**

Run: `cd product && go test ./internal/web/...`
Expected: PASS.

---

### Task 6: Full verification + visual review

- [ ] **Step 1: vet + full suite**

Run: `cd product && go vet ./... && go test ./...`
Expected: all PASS.

- [ ] **Step 2: Browser pass at 375 / 768 / 1280**

Restart the dev server (`ADMIN_EMAIL=rasmus@example.com ./bin/server` after `go build -o bin/server ./cmd/server`), then screenshot: landing, dashboard (empty), intake tiles, concept choice, footer detail — at all three widths. Expected: no collision in footer, no purple anywhere, duotone art on tiles/mockups, hero demo present and well-composed, structured summary blocks, empty-state card.

- [ ] **Step 3: Keyboard walk**

Tab through landing + intake: nav burger opens with Space, tile radios reachable and show the gold focus ring, footer links all reachable.

- [ ] **Step 4: Show before/after in the visual companion and report**

- [ ] **Step 5: Commit (ONLY with the user's explicit approval)**
