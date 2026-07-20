# Forge control plane — UI refinement (P0 defects + P1 conversion surfaces)

Date: 2026-07-20
Status: approved by user (scope P0+P1; cleared to edit despite concurrent session)

## Context

Follow-up to the design-variety prompt surgery (2026-07-19 spec). A browser audit
of the Forge control plane (landing → login → dashboard → new project → intake
tiles → concept choice → admin, dev mode) found five defects. This is a
refinement inside the existing design system (tokens, JetBrains Mono voice,
quality floor) — not a redesign. Admin (operator-only) and all flows/logic stay
untouched. All customer-facing strings stay i18n'd (en/sv/ru parity test).

## Defects and fixes

### P0-1 — Footer locale collision (every page)

`partials.html` renders `.foot` as two inline spans; the locale switcher runs
into the withdrawal link ("…Withdraw from a purchase English Svenska Русский").

Fix (app.css only, markup unchanged): `.foot` becomes a centered flex column
with `--sp-2` gap; `.lang-switch` gets its own row with a hairline top border.

### P0-2 — Foreign hues injected into preview palettes

`previewPaletteVars` (handlers_projects.go) tops up missing palette slots from
`{"#F5F1E8", "#171713", "#D4FF3F", "#FFFFFF", "#8277FF"}` — cream + acid-green +
purple, the exact AI tells banned pipeline-side. Every 4-color palette (all
current intake options) gets the purple `--pv-accent-2`, rendered as a purple
blob on tiles and concept mockups.

Fix: neutral non-cluster fallbacks `{"#F5F6F4", "#161A18", "#2457D6", "#FFFFFF", ""}`;
a missing accent-2 derives from the palette itself as
`color-mix(in srgb, var(--pv-accent) 55%, var(--pv-bg))` (emitted as the custom
property value; browser computes it). No foreign hue can leak in.

### P0-3 — Blob art → duotone media-frame art

`.dt-art::before/::after` and `.concept-visual::before/::after` render two
random organic blobs regardless of direction. Replace the fill language (same
geometry, same layout variants) with a composed, palette-derived device:

- `::before` — a rounded media panel with a diagonal duotone gradient built
  from accent→ink/surface color-mixes (reads as a color-graded image slot).
- `::after` — a crisp circular "subject crop" in `--pv-surface` with a thin
  `--pv-accent-2` ring and soft ink shadow (the signature circular close-up).
- The existing `i` baseline rule (caption bar) stays.

### P1-4 — Landing hero shows a real product artifact

The landing sells websites and shows none; the real-examples showcase is
deliberately empty until the owner supplies real screenshots (never invented
proof). Fix: keep the hero copy as-is and add a full-width product artifact below it —
a static desktop+mobile hero mockup using the existing
`concept-device-pair`/`concept-desktop`/`concept-mobile` classes — dogfooding
the product's own artifact, with the new duotone art. Palette scoped via a
`.hero-demo` rule (CSP bans inline styles) using the Signal & paper values;
clearly labelled with a localized "Sample preview" badge (new key
`landing.demo.badge` ×3 locales); device pair is `aria-hidden` (illustrative).
Sample copy is generic-demo, not a claim about a real business. Mobile: single
column, demo below copy (uses the existing mobile concept-pair rules).

### P1-5 — Run-on summary blocks

`.Answers` ("Q\n→ A" pairs) and `.DesignBrief` (newline-joined fields) already
contain line breaks; `<p class="brief">` collapses them. One-rule fix:
`.brief { white-space: pre-line; }`.

### P1-6 — Dashboard empty state

Replace the bare "No projects yet" line with a proper empty-state card (dashed
border, centered, title + support line + primary button reusing the existing
`dash.empty`/`dash.empty_link` strings). One new support-line key
`dash.empty_sub` ×3 locales.

## Explicitly out of scope

- Step cards, pricing card, admin, wide-screen container rethink, any logic/flow.
- Parsing hexes out of DesignBrief into swatches (intrusive, brittle).

## Files

- `internal/web/handlers_projects.go` — previewPaletteVars
- `internal/web/static/app.css` — foot/lang-switch, dt-art/concept-visual art,
  .brief, .empty-state, .hero grid + .hero-demo (and mobile block additions)
- `internal/web/templates/landing.html` — hero grid + demo markup
- `internal/web/templates/dashboard.html` — empty-state markup
- `internal/web/i18n/locales/{en,sv,ru}.json` — landing.demo.badge, dash.empty_sub

## Verification (AGENTS.md definition of done)

- `go vet ./... && go test ./...` green (i18n parity test covers the new keys).
- Browser pass at 375/768/1280 of: landing, dashboard (empty), project intake
  tiles, concept choice, any footer page; keyboard-only walk (nav burger, tile
  radios, footer links); `prefers-reduced-motion` unaffected (no new animation).
- Show before/after in the visual companion for user review.

## Risks

- Concurrent session edits the same files — user confirmed clear to edit.
- Tests asserting palette.css content: only the `--pv-bg:#F5F6F4` prefix is
  asserted (still true). No test pins #8277FF/#D4FF3F.
