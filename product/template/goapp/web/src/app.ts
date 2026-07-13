// ALL client-side JavaScript for the public site lives in this one file. It is
// compiled to /static/app.js by `make js` (serve.sh and `make test` run it for
// you — never edit app.js by hand) and type-checked with tsc --strict.
//
// Rules:
//   - No frameworks, no npm imports — plain, typed DOM code only.
//   - Progressive enhancement only: every feature must still WORK without JS
//     (forms post, links navigate). This file may only make things smoother.
//   - Most sites ship this file EMPTY. The mobile nav is CSS-only and page
//     transitions are native (components.css). Reach for TS only when the plan
//     genuinely needs an interactive widget: a gallery/lightbox, a date picker,
//     live filtering — and keep it small.

export {};
