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
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/project"
)

// SetPreviewDomain turns on branded preview URLs under domain (e.g.
// "forge.transcendsoftware.se"). Empty keeps previews on their direct URLs.
func (o *Orchestrator) SetPreviewDomain(domain string) {
	o.previewDomain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
}

// SetPreviewSelfProbe points branded-preview verification at our OWN HTTP
// listener (e.g. "http://127.0.0.1:8080"). Probing the public branded URL from
// inside Fly hairpins through our own dedicated edge IP — a route that proved
// flaky from this vantage point while working fine from the customer's — and
// the static parts of the chain (wildcard DNS, cert, edge routing) don't need
// per-build re-verification anyway. What DOES need it per build is ours:
// the persisted host resolving through the reverse proxy to a live backend —
// exactly what a localhost request with the branded Host header exercises.
func (o *Orchestrator) SetPreviewSelfProbe(baseURL string) {
	o.previewSelfProbe = strings.TrimRight(baseURL, "/")
}

// PreviewDomain reports the configured branded-preview domain ("" = off).
func (o *Orchestrator) PreviewDomain() string { return o.previewDomain }

// brandedPreviewURL finalizes the customer-facing preview URL after a verified
// deploy: assign a stable preview host once, verify the branded URL end-to-end
// (DNS wildcard → Forge proxy → site), and return it. On failure — typically
// the wildcard DNS or cert not being live yet — it returns the direct URL
// unchanged and alerts the operator: a build must never break on our own
// front-door plumbing. On a first preview the assigned host is persisted here
// (so our own proxy can resolve it during verification); the caller still saves
// p afterward with the final PreviewURL.
func (o *Orchestrator) brandedPreviewURL(ctx context.Context, p *project.Project, directURL string) string {
	if o.previewDomain == "" || directURL == "" {
		return directURL
	}
	if p.PreviewHost == "" {
		p.PreviewHost = o.newPreviewHost(ctx, p)
		// Persist the host BEFORE verifying: the branded URL is served by OUR
		// reverse proxy, which resolves it via ProjectByPreviewHost. On a project's
		// first preview the row must already be in the store, or the probe 404s and
		// we needlessly fall back to the fly.dev URL (and alert the operator that
		// the DNS/cert is broken when it isn't).
		if err := o.save(ctx, p); err != nil {
			o.log.Error("branded preview: persist host before verify", "project", p.ID, "err", err)
		}
	}
	branded := "https://" + p.PreviewHost + "." + o.previewDomain
	// The direct URL already verified, so the site is up — give our proxy path a
	// short window, not the full deploy window.
	vctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if err := o.verifyBranded(vctx, p.PreviewHost); err != nil {
		o.log.Error("branded preview host not reachable — serving the direct URL",
			"project", p.ID, "branded", branded, "err", err)
		o.notifyOperator(ctx, "Forge: branded preview host failed",
			fmt.Sprintf("%s did not come up for %q (falling back to %s).\n"+
				"Probe error: %v\n"+
				"The healer retries every reaper tick and upgrades the URL when the host answers; "+
				"if this repeats, check the wildcard DNS record and certificate for *.%s.\n\n%s",
				branded, p.Name, directURL, err, o.previewDomain, o.projectLink(p.ID)))
		return directURL
	}
	return branded
}

// verifyBranded checks that the branded host serves. With a self-probe
// configured it asks our OWN listener with the branded Host header — the
// per-build chain (persisted host → reverse proxy → live backend) without the
// flaky in-Fly hairpin through the public edge. Otherwise it probes the public
// URL via the verifier (dev, tests).
func (o *Orchestrator) verifyBranded(ctx context.Context, host string) error {
	branded := "https://" + host + "." + o.previewDomain
	if o.previewSelfProbe == "" {
		return o.verifier.Verify(ctx, branded)
	}
	client := &http.Client{
		Timeout: 10 * time.Second,
		// The proxied app may 303 to /login (absolute URL on the branded host) —
		// following it would dial the public edge, the exact path we're avoiding.
		// The redirect itself already proves the proxy resolved and the backend
		// answered.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	var lastErr error
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.previewSelfProbe+"/", nil)
		if err != nil {
			return err
		}
		req.Host = host + "." + o.previewDomain // previewProxy branches on this
		resp, err := client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if resp.StatusCode < 400 {
				return nil
			}
			err = fmt.Errorf("status %d via self-probe", resp.StatusCode)
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return fmt.Errorf("branded host not serving: %w", lastErr)
		case <-time.After(3 * time.Second):
		}
	}
}

// healBrandedPreviews retries the branded host for projects stuck on their
// direct fly.dev URL. The build-time probe gets only 45 seconds — when it
// loses the race (cold path, edge propagation), the customer used to keep the
// internal URL until the next restart's backfill happened to fix it. Called
// every reaper tick; verifies before flipping, so a genuinely broken host
// changes nothing.
func (o *Orchestrator) healBrandedPreviews(ctx context.Context) {
	if o.previewDomain == "" {
		return
	}
	ps, err := o.store.Projects(ctx)
	if err != nil {
		return
	}
	for _, p := range ps {
		if p.PreviewURL == "" || p.PreviewHost == "" || !strings.Contains(p.PreviewURL, ".fly.dev") {
			continue
		}
		branded := "https://" + p.PreviewHost + "." + o.previewDomain
		vctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := o.verifyBranded(vctx, p.PreviewHost)
		cancel()
		if err != nil {
			continue
		}
		p.PreviewURL = branded
		if err := o.save(ctx, p); err != nil {
			o.log.Error("preview heal: save", "project", p.ID, "err", err)
			continue
		}
		o.log.Info("preview heal: branded host recovered", "project", p.ID, "url", branded)
	}
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
