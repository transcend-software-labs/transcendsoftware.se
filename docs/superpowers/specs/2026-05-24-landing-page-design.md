# Transcend Software Landing Page & Blog

## Overview

Personal landing page + blog for Rasmus Kockum, Senior Software Engineer & Tech Lead at Transcend Software. Built with Astro. Dark theme, Hellsoft-inspired design with restrained color strategy. Replaces existing transcendsoftware.se.

## Architecture

- **Framework**: Astro (static site generation)
- **Styling**: CSS with OKLCH colors, no CSS framework
- **Content**: Markdown blog posts with frontmatter
- **Deployment**: Static export, served from transcendsoftware.se

## Design System

### Theme: Dark
- Background: OKLCH near-black with warm tint (~oklch(0.12 0.005 85))
- Text: OKLCH off-white (~oklch(0.92 0.005 85))
- Accent: Restrained warm amber/gold on <=10% of surface
- No: #000, #fff, cards, side-borders, gradient text, glassmorphism, em dashes

### Typography
- Headings: Monospace (JetBrains Mono or similar)
- Body: Sans-serif (Inter or system stack)
- Body line length: 65-75ch max
- Scale: >=1.25 ratio between steps

### Layout
- Fixed nav at top
- Full-width sections, no containers wrapping everything
- Varied spacing for rhythm
- No card grids

## Pages & Structure

### Landing Page (index.astro)
Sections top-to-bottom:
1. **Nav** — Fixed, `Transcend Software` left, links right (Services | Work | Posts | Contact), dark/light toggle
2. **Hero** — Name (mono), title, one-liner, CTA "What I do ↓"
3. **Services** — 3 services: Senior Engineer, Tech Lead & AI Coach, Software Architect
4. **Specialisms** — Dense tech tags (#Java #Spring Boot #Golang #TypeScript etc)
5. **Work** — Numbered project list with lorem ipsum placeholders (6 projects)
6. **Contact** — Email link, location, LinkedIn, GitHub, availability status
7. **Footer** — Copyright, built with Astro, version

### Blog
- `/posts` — List all posts with featured and recent
- `/posts/[slug]` — Individual post (preserve existing URLs for SEO)
- `/tags` — Tag index
- `/tags/[tag]` — Posts by tag
- RSS feed at `/rss.xml`

## SEO
- Preserve existing URL structure (/posts/slug)
- Meta tags, OG tags, Twitter cards on all pages
- Sitemap.xml
- robots.txt
- Canonical URLs
- Structured data (BlogPosting schema)

## Data
- Blog posts: Migrated from existing transcendsoftware.se (~7 posts)
- Projects: Lorem ipsum placeholders, user will fill in
- Skills: Java, Spring Boot, Golang, CI/CD (GitHub Actions, Terraform), Kubernetes, JavaScript, React, AI setups

## Implementation Order
1. Scaffold Astro project
2. Design system CSS + layout
3. Landing page sections
4. Blog system
5. Migrate existing posts
6. SEO
7. Playwright verification
8. Push to Transcend Software Labs org
