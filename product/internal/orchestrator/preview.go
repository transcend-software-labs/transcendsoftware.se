package orchestrator

// Branded preview URLs. Preview apps deploy to their own Fly apps
// (forge-<id>.fly.dev), but customers should never see that internal shape —
// when a preview domain is configured, previews are handed out as
// https://<slug>-<shortid>.<previewDomain> and served through the web layer's
// reverse proxy (internal/web/preview_proxy.go). The direct fly.dev URL keeps
// working (in-sandbox review, verification, fallback); it's just not shown.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/project"
)

// SetPreviewDomain turns on branded preview URLs under domain (e.g.
// "forge.transcendsoftware.se"). Empty keeps previews on their direct URLs.
func (o *Orchestrator) SetPreviewDomain(domain string) {
	o.previewDomain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
}

// PreviewDomain reports the configured branded-preview domain ("" = off).
func (o *Orchestrator) PreviewDomain() string { return o.previewDomain }

// brandedPreviewURL finalizes the customer-facing preview URL after a verified
// deploy: assign a stable preview host once, verify the branded URL end-to-end
// (DNS wildcard → Forge proxy → site), and return it. On failure — typically
// the wildcard DNS or cert not being live yet — it returns the direct URL
// unchanged and alerts the operator: a build must never break on our own
// front-door plumbing. p is mutated (PreviewHost) but not saved; the caller
// persists it.
func (o *Orchestrator) brandedPreviewURL(ctx context.Context, p *project.Project, directURL string) string {
	if o.previewDomain == "" || directURL == "" {
		return directURL
	}
	if p.PreviewHost == "" {
		p.PreviewHost = o.newPreviewHost(ctx, p)
	}
	branded := "https://" + p.PreviewHost + "." + o.previewDomain
	// The direct URL already verified, so the site is up — give our proxy path a
	// short window, not the full deploy window.
	vctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if err := o.verifier.Verify(vctx, branded); err != nil {
		o.log.Error("branded preview host not reachable — serving the direct URL",
			"project", p.ID, "branded", branded, "err", err)
		o.notifyOperator(ctx, "Forge: branded preview host failed",
			fmt.Sprintf("%s did not come up for %q (falling back to %s).\n"+
				"Check the wildcard DNS record and certificate for *.%s.\n\n%s",
				branded, p.Name, directURL, o.previewDomain, o.projectLink(p.ID)))
		return directURL
	}
	return branded
}

// BackfillPreviewHosts rewrites existing direct (fly.dev) preview URLs to the
// branded host, assigning PreviewHost where missing. Idempotent — run at
// startup once the wildcard DNS is live (the caller gates on a canary probe);
// projects built before the feature keep working links either way, this just
// stops the internal URLs from being shown.
func (o *Orchestrator) BackfillPreviewHosts(ctx context.Context) {
	if o.previewDomain == "" {
		return
	}
	ps, err := o.store.Projects(ctx)
	if err != nil {
		o.log.Error("preview backfill: list projects", "err", err)
		return
	}
	n := 0
	for _, p := range ps {
		if p.PreviewURL == "" || !strings.Contains(p.PreviewURL, ".fly.dev") {
			continue
		}
		if p.PreviewHost == "" {
			p.PreviewHost = o.newPreviewHost(ctx, p)
		}
		p.PreviewURL = "https://" + p.PreviewHost + "." + o.previewDomain
		if err := o.save(ctx, p); err != nil {
			o.log.Error("preview backfill: save", "project", p.ID, "err", err)
			continue
		}
		n++
	}
	if n > 0 {
		o.log.Info("preview backfill: rewrote direct URLs to the branded host", "projects", n)
	}
}

// newPreviewHost picks the project's preview subdomain label:
// slug(name)-<id[:6]>, extending the id suffix on the (vanishingly rare)
// collision with another project so the unique index never fails a build.
func (o *Orchestrator) newPreviewHost(ctx context.Context, p *project.Project) string {
	for _, n := range []int{6, 10, 16} {
		host := previewHost(p.Name, p.ID, n)
		other, err := o.store.ProjectByPreviewHost(ctx, host)
		if err != nil || other.ID == p.ID { // free (ErrNotFound) or already ours
			return host
		}
	}
	return previewHost(p.Name, p.ID, 32) // full id — unique by construction
}

// previewHost builds the subdomain label: a DNS-safe slug of the project name
// plus the first idChars of the project id ("bageriet-a1fa81"). Falls back to
// "site" when the name has no usable characters. The label stays well under
// DNS's 63-char limit.
func previewHost(name, id string, idChars int) string {
	slug := slugifyHost(name)
	if slug == "" {
		slug = "site"
	}
	if len(slug) > 30 {
		slug = strings.Trim(slug[:30], "-")
	}
	if idChars > len(id) {
		idChars = len(id)
	}
	return slug + "-" + strings.ToLower(id[:idChars])
}

// slugifyHost reduces a project name to a DNS-usable label (å/ä→a, ö→o, keep
// [a-z0-9-]). "" when nothing usable remains. (Same rules as the domain-search
// slugify in internal/hostup, which is unexported there.)
func slugifyHost(q string) string {
	q = strings.ToLower(strings.TrimSpace(q))
	var b strings.Builder
	for _, r := range q {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-':
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('-')
		case r == 'å' || r == 'ä':
			b.WriteByte('a')
		case r == 'ö':
			b.WriteByte('o')
			// anything else is dropped
		}
	}
	// collapse runs of hyphens from spaced/punctuated names
	s := b.String()
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	return strings.Trim(s, "-")
}
