package web

// Branded preview hosts. When PREVIEW_DOMAIN is set, customer previews are
// handed out as https://<label>.<PREVIEW_DOMAIN> (label = Project.PreviewHost,
// assigned by the orchestrator) and served here: requests whose Host falls
// under the preview domain are reverse-proxied to the project's own Fly app,
// so the internal fly.dev URLs never reach the customer. A wildcard cert on
// the Forge app terminates TLS; DNS is a wildcard CNAME to the app.

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/transcend-software-labs/rasmus-ai/internal/builder"
	"github.com/transcend-software-labs/rasmus-ai/internal/project"
	"github.com/transcend-software-labs/rasmus-ai/internal/web/i18n"
)

// previewProxy branches preview-host requests off before the normal Forge
// routes (and before langSelector — ?lang= must not redirect a preview).
// Everything not under the preview domain falls through untouched.
func (s *Server) previewProxy(next http.Handler) http.Handler {
	domain := strings.ToLower(s.cfg.PreviewDomain)
	if domain == "" {
		return next
	}
	suffix := "." + domain
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := strings.ToLower(stripPort(r.Host))
		if host == domain || !strings.HasSuffix(host, suffix) {
			next.ServeHTTP(w, r)
			return
		}
		s.servePreview(w, r, strings.TrimSuffix(host, suffix))
	})
}

// stripPort removes a :port from a Host header value (present in dev).
func stripPort(host string) string {
	if i := strings.LastIndexByte(host, ':'); i >= 0 && !strings.Contains(host[i:], "]") {
		return host[:i]
	}
	return host
}

// servePreview resolves the subdomain label to a project and proxies to its
// Fly app. Unknown labels 404; expired previews get a branded page (the reaper
// has destroyed the app by then).
func (s *Server) servePreview(w http.ResponseWriter, r *http.Request, label string) {
	p, err := s.store.ProjectByPreviewHost(r.Context(), label)
	if err != nil {
		s.renderPreviewGone(w, http.StatusNotFound, "en", "preview.notfound_title", "preview.notfound_body")
		return
	}
	if p.Status == project.StatusExpired || p.PreviewURL == "" {
		s.renderPreviewGone(w, http.StatusGone, p.Locale, "preview.gone_title", "preview.gone_body")
		return
	}
	s.previewBackend(p).ServeHTTP(w, r)
}

// previewBackend builds the reverse proxy for one project's site.
func (s *Server) previewBackend(p *project.Project) *httputil.ReverseProxy {
	target := s.previewTargetURL(p.ID)
	branded := p.PreviewHost + "." + strings.ToLower(s.cfg.PreviewDomain)
	locale, paid := p.Locale, p.Paid
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target) // scheme+host → backend; outbound Host follows (Fly routes by Host/SNI)
			pr.SetXForwarded()
		},
		ModifyResponse: func(resp *http.Response) error {
			// A backend redirect to its own absolute URL must not leak the
			// internal host — point it back at the branded one.
			if loc := resp.Header.Get("Location"); loc != "" {
				if lu, err := url.Parse(loc); err == nil && lu.Host == target.Host {
					lu.Scheme, lu.Host = "https", branded
					resp.Header.Set("Location", lu.String())
				}
			}
			// Unpaid previews stay out of search engines; a paid customer's
			// delivered site on the branded host is theirs to index.
			if !paid {
				resp.Header.Set("X-Robots-Tag", "noindex")
			}
			return nil
		},
		FlushInterval: -1, // stream responses as they come
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			s.log.Error("preview proxy", "project", p.ID, "backend", target.Host, "err", err)
			s.renderPreviewGone(w, http.StatusBadGateway, locale, "preview.error_title", "preview.error_body")
		},
	}
}

// previewTargetURL is the backend origin for a project's site. A var so tests
// can point the proxy at an httptest server.
func (s *Server) previewTargetURL(projectID string) *url.URL {
	if s.previewTarget != nil {
		return s.previewTarget(projectID)
	}
	return &url.URL{Scheme: "https", Host: builder.DeployAppName(projectID) + ".fly.dev"}
}

// renderPreviewGone writes the minimal standalone page shown when a preview
// host has nothing to serve (unknown, expired, or backend down). Everything is
// inlined — on a preview host, asset paths would route to the backend.
func (s *Server) renderPreviewGone(w http.ResponseWriter, status int, lang, titleKey, bodyKey string) {
	if lang == "" {
		lang = "en"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Robots-Tag", "noindex")
	w.WriteHeader(status)
	data := map[string]any{
		"Title": i18n.T(lang, titleKey),
		"Body":  i18n.T(lang, bodyKey),
		"Lang":  lang,
	}
	if err := s.tmpl.ExecuteTemplate(w, "preview_gone", data); err != nil {
		s.log.Error("render preview_gone", "err", err)
	}
}
