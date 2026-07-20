# Transcend Forge — platform conventions

This file governs work on the **Forge control plane** (this Go app: the
customer dashboard, project pages, billing, admin). It is NOT about the
generated customer sites — those follow `template/goapp/AGENTS.md` and the
`frontend-design` skill baked into the starter.

## UI design system (internal/web/)

The control panel practices what the product preaches: intentional tokens, a
quality floor (keyboard focus, responsive, reduced motion, ONE spacing scale),
and copy treated as design. The full design surface lives in
`internal/web/static/app.css` — a single stylesheet, no framework, no bundler.
References: the vendored `frontend-design` skill
(`template/goapp/.opencode/skills/frontend-design/SKILL.md`) and Vercel's web
interface guidelines (vercel.com/design/guidelines).

### Tokens — never hardcode what a token covers

Everything visual comes from `:root` in `app.css`:

- **Palette**: `--bg/--bg-2/--surface/--surface-2/--text/--muted/--dim/
  --accent/--accent-2/--accent-hi/--accent-lo/--accent-ink/--green/--red/
  --red-soft/--border/--border-2`. Dark + gold is the brand; there is no light
  mode. `--dim` is the darkest text allowed on any surface (AA-checked).
- **Tints**: translucent brand color comes ONLY from `--accent-glow/-fill/
  -border`, `--green-fill`, `--red-fill`, `--focus-ring`. Never write
  `rgba(224,169,60,…)` or ad-hoc `color-mix` for a tint that exists.
- **Spacing**: margins/gaps/padding from `--sp-1..8` (.25→4rem);
  buttons/inputs use `--control-py/--control-px`. Fine optical nudges
  (±.1–.2rem) are allowed; new ad-hoc rhythm values are not.
- **Type scale**: every font-size from `--fs-xs..--fs-3xl` (12px floor —
  nothing smaller, ever). The hero clamp is the one sanctioned exception.
- **Z-index**: `--z-veil/-nav/-menu`. **Radii**: `--radius`(8)/`--radius-lg`(14);
  child radius ≤ parent (`calc(var(--radius) - 2px)` for nested thumbs).

### Typography — sans for reading, mono for voice

- Body text renders in `--font-sans` (system stack; never load a body webfont).
- JetBrains Mono (400/500/600 only — 700 is NOT loaded, never use it) carries
  identity + data: headings, `.brand`, `.btn`, `.badge`, tables, `.plan/.log`,
  prices, timeline, paths, `code`. The scope list is the rule at the END of
  app.css — extend it there, don't sprinkle `font-family` locally.
- Hierarchy: `h1` page anchor, `h2` panel title, `h3` = mono eyebrow
  (uppercase 12px kicker). Don't inline-style headings.
- Numbers that get compared line-to-line use `tabular-nums` (tables/stats/prices).

### Components — compose, don't re-invent

- **Badges**: `.badge` base + `badge-<status>` (dynamic from project status),
  `badge-paid/-unpaid`, `badge-accent`, `badge-sev`, `badge-lg`. New badge =
  new modifier on `.badge`, never a parallel pill class.
- **Buttons**: `.btn` (gold, primary — one per view ideally), `.btn-sm/-lg`,
  and `.btn-ghost` (+`-accent`/`-danger`) for secondary/destructive. A ghost
  variant is `--ghost`-driven; don't hand-undo the gold gradient again.
- **Cards/panels**: `.panel` = section rhythm (top hairline). `.card` = bordered
  surface. Compose (`class="panel card"`) — never re-add source-order override
  hacks. `.sub-panel`/`.statusbox` etc. only layer identity on top.
- **Tables**: `.table` inside `.table-scroll` (+ `.table-nowrap`).
- **Empty states**: `.empty-state` (dashed card, centered) with `.empty-title`;
  recompose per surface, don't invent parallel empty-box styles.
- **Preview mockups**: the intake/concept previews (`.design-tile-preview`,
  `.concept-device-pair`) get their palette from `--pv-*` vars (derived in
  `handlers_projects.go previewPaletteVars` — never inject hues the model
  didn't choose) and their art from ONE device: duotone media panel + circular
  crop (`.dt-art`/`.concept-visual` `::before/::after`). The landing's
  `.hero-demo` dogfoods the same mockup with a scoped sample palette.
- **Notices**: `.notice` (+ a keeper class for layout extras).
- **Forms**: inputs style themselves from the base `input, textarea` rule —
  component rules add flex-sizing only. Every control has a `<label>`,
  `autocomplete` where meaningful, `spellcheck=false` for emails/codes.
- **Busy state**: submit buttons get `.is-busy` automatically via the
  partials.html foot script (keep-label + spinner). Opt out: `data-no-busy`.

### Quality floor — non-negotiable on every change

- **Keyboard**: everything focusable shows the gold `:focus-visible` ring;
  card-shaped radio rows surface it via `:has(input:focus-visible)`. Never
  `display:none` a control that must stay reachable (`.sr-only` instead — see
  the nav burger).
- **Touch**: ≥44px targets on mobile (the trailing media block enforces it for
  `.btn`/`.linklike` — keep new controls covered).
- **Contrast**: text ≥ AA. `--dim` is the floor; never introduce a darker text
  color. Status is never color-alone (badges carry text, timeline carries glyphs).
- **Mobile**: ONE `@media (max-width: 720px)` block, kept at the END of app.css
  (media queries win by source order). 16px inputs (iOS zoom). Test at 375px.
- **Motion**: `prefers-reduced-motion` globally disables animation — keep it.
  Animate `transform`/`opacity` only; never `transition: all`.
- **Head**: every page ships favicon, `meta description` (i18n `meta.desc`),
  `theme-color`, `color-scheme: dark`, OG tags — via the `head` partial only.
- **Assets**: htmx + the SSE extension are vendored in `static/` (pinned
  versions). No runtime CDN dependencies. Fonts load via `<link>` +
  preconnect, never CSS `@import`.

### Copy

- All customer-facing strings go through i18n (`internal/web/i18n/locales/`,
  en/sv/ru — the parity test fails the build if a locale is missed). Admin
  pages are English-only by design.
- Curly quotes (’ “ ”) and the ellipsis character (…), not straight quotes/`...`.
  Active voice, second person, concrete verbs ("Save changes", not "Submit").
  Errors say what went wrong AND what to do next.
- Prices/plan behavior are disclosed before payment, in the customer's words
  ("changes", never tokens/AI cost).

### Guardrails when changing templates

Some tests assert on rendered strings — grep `internal/web/*_test.go` before
rewording: `id="livelog"`, `sse-connect=`, "Design audit",
"Danger zone", "Unpaid", price strings ("99 kr", "29 kr/mo", "49 kr"),
`name="domain_mode"`. htmx swap targets (`#domain-panel`,
`#sub-domain-results`, `#domain-results`, the status poll) are load-bearing ids.

### Definition of done for UI work

`go vet ./... && go test ./...` green, then a browser pass at 375/768/1280px
of every state the change touches, plus a keyboard-only walk of any new
interaction. If it isn't verified in a browser, it isn't done.
