---
target: homepage and blog
total_score: 29
p0_count: 2
p1_count: 2
timestamp: 2026-05-27T19-27-19Z
slug: src-pages-index-astro
---
## Design Health Score

| # | Heuristic | Score | Key Issue |
|---|-----------|-------|-----------|
| 1 | Visibility of System Status | 3 | Available status exists and is green. All homepage anchor links are simultaneously marked active. |
| 2 | Match System/Real World | 4 | Language matches how engineers think. No jargon inflation. |
| 3 | User Control and Freedom | 3 | Smooth scroll nav works. No Escape key or click-outside to close mobile menu. |
| 4 | Consistency and Standards | 2 | Bracket-label section headers conflict with the brand. Inline style overrides in blog templates break token contract. |
| 5 | Error Prevention | 3 | External links missing rel="noopener noreferrer". |
| 6 | Recognition Rather Than Recall | 3 | Email visible in Contact without interaction. No skip-to-content link. |
| 7 | Flexibility and Efficiency | 3 | RSS exists. No skip-to-main link for keyboard nav. |
| 8 | Aesthetic and Minimalist Design | 3 | Strong restraint overall. 19-tag Specialisms wall and dual-listed blog posts add unnecessary noise. |
| 9 | Error Recovery | 2 | 404 page has no h1. |
| 10 | Help and Documentation | 3 | "Currently available" contradicts "Q1 2027" — one is wrong. |
| **Total** | | **29/40** | **Good. Clear improvement vectors.** |

## Anti-Patterns Verdict

No gradient text, no glassmorphism, no hero-metric templates, no icon+heading+text card grids. These bans are cleanly avoided.

Bracket-label section headers ([ 01 // Services ], SVC / 01) are the single most recognized AI-template signature in 2025. This is the largest credibility risk on the site for the target audience.

## Overall Impression

The site's bones are right. The hero subtitle is the best thing on the page. The conversion path is structurally clean. The biggest problem is framing hardware — section labels, service codes, tag wall — that reads as scaffolding left in.

## Priority Issues

**[P0] --text-dim fails WCAG AA contrast (~3.3:1 vs 4.5:1 required)**
- Affects nav links, hero CTA, contact field labels, section labels, post metadata, back-navigation.
- Fix: raise --text-dim to ~oklch(0.55 0.005 85) and adjust --text-muted to maintain hierarchy.

**[P0] "Currently available" contradicts "Available Q1 2027"**
- Both statements cannot be true. Trust damage at the exact moment of conversion.
- Fix: pick one truth and apply consistently.

**[P1] Bracket-label section headers signal AI-generated template**
- [ 01 // Services ], [ 02 // Technology ], SVC / 01-03.
- Fix: remove brackets and slashes. Plain numbering or no numbering.

**[P1] Posts index and 404 page have no h1**
- Semantic and SEO gap. Blog is justified partly by SEO value.
- Fix: add h1 as primary heading on both pages.

**[P2] Specialisms section is a credibility dilution**
- 19 undifferentiated tags. Intro duplicates Services copy verbatim.
- Fix A: delete the section. Fix B: 3 labeled groups of 5-7 items.

**[P2] Work item descriptions are uneven**
- Items 04, 05, 06 lack specificity or outcomes. Dilute adjacent strong entries.
- Fix: add outcome/scale to each weak entry.

## Minor Observations

- rel="noopener noreferrer" missing on external links.
- Nav active state marks all homepage anchors simultaneously — useless as location indicator.
- Blog index heading reads as meta description, not authored copy.
- Post article h1 uses inline style, bypassing CSS token system.
- Footer v.2026.05 is unlabeled and wasted.
- --status-available color hardcoded instead of using a CSS variable.
