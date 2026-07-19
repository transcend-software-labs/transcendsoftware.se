package web

// Forge's own SEO surface (the generated sites carry their own — see
// template/goapp/internal/web/seo.go). The public face is tiny — the landing
// page and the terms — so this is mostly about being found and previewing well:
// canonical URLs pinned to BaseURL (the app also answers on its fly.dev host),
// Open Graph/Twitter tags with a real card image, JSON-LD, and the crawl pair.
//
// Preview hosts never reach these handlers: previewProxy branches on Host
// before the mux, so slug.forge.… serves the generated site's own
// robots/sitemap (with X-Robots-Tag: noindex until paid).

import (
	"encoding/json"
	"html"
	"html/template"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/transcend-software-labs/rasmus-ai/internal/web/i18n"
)

// publicPages are the crawlable URLs listed in /sitemap.xml. Everything else
// is either auth-gated or an auth page — nothing worth indexing.
var publicPages = []string{"/", "/terms", "/privacy"}

func isPublicPage(path string) bool {
	for _, page := range publicPages {
		if path == page {
			return true
		}
	}
	return false
}

type alternateLink struct {
	Lang string
	URL  string
}

// localizedPublicURL gives every translated public page a stable crawlable URL
// while preserving the short English canonical. Query URLs are intentional:
// they fit the existing zero-JS language selector without duplicating routes.
func localizedPublicURL(origin, path, lang string) string {
	base := origin + path
	if lang == "" || lang == i18n.Default {
		return base
	}
	return base + "?lang=" + lang
}

func (s *Server) alternateLinks(r *http.Request) []alternateLink {
	if !isPublicPage(r.URL.Path) {
		return nil
	}
	origin := s.origin(r)
	links := make([]alternateLink, 0, len(i18n.Langs)+1)
	for _, lang := range i18n.Langs {
		links = append(links, alternateLink{Lang: lang.Code, URL: localizedPublicURL(origin, r.URL.Path, lang.Code)})
	}
	links = append(links, alternateLink{Lang: "x-default", URL: localizedPublicURL(origin, r.URL.Path, i18n.Default)})
	return links
}

// origin is the site's canonical origin: BaseURL when configured (so pages
// served via the fly.dev host still canonicalize to the real domain), else
// derived from the request (dev, tests).
func (s *Server) origin(r *http.Request) string {
	if base := strings.TrimRight(s.cfg.BaseURL, "/"); base != "" {
		return base
	}
	scheme := "https"
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = p
	} else if strings.HasPrefix(r.Host, "localhost") || strings.HasPrefix(r.Host, "127.0.0.1") {
		scheme = "http"
	}
	return scheme + "://" + r.Host
}

// orgNode is the schema.org Organization shared by the site-wide JSON-LD and
// the landing page's richer graph.
func (s *Server) orgNode(origin string) map[string]any {
	return map[string]any{
		"@type": "Organization",
		"name":  "Transcend Forge",
		"url":   origin + "/",
	}
}

// siteJSONLD is the structured data every page carries.
func (s *Server) siteJSONLD(r *http.Request) template.JS {
	node := s.orgNode(s.origin(r))
	node["@context"] = "https://schema.org"
	b, err := json.Marshal(node)
	if err != nil {
		return "" // never break a page over metadata
	}
	return template.JS(b) // marshaled by us, so safe inside <script>
}

// landingJSONLD upgrades the landing page to an Organization + Service graph,
// with a real Offer when the live Stripe price is known (amountMinor in öre).
func (s *Server) landingJSONLD(r *http.Request, amountMinor int64, currency string) template.JS {
	origin := s.origin(r)
	svc := map[string]any{
		"@type":       "Service",
		"name":        "Transcend Forge",
		"serviceType": "Website design, development and hosting",
		"url":         origin + "/",
		"provider":    s.orgNode(origin),
	}
	if amountMinor > 0 && currency != "" {
		svc["offers"] = map[string]any{
			"@type":         "Offer",
			"price":         strconv.FormatFloat(float64(amountMinor)/100, 'f', -1, 64),
			"priceCurrency": strings.ToUpper(currency),
		}
	}
	b, err := json.Marshal(map[string]any{"@context": "https://schema.org", "@graph": []any{svc}})
	if err != nil {
		return ""
	}
	return template.JS(b)
}

// handleSitemap serves /sitemap.xml from publicPages.
func (s *Server) handleSitemap(w http.ResponseWriter, r *http.Request) {
	origin := s.origin(r)
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9" xmlns:xhtml="http://www.w3.org/1999/xhtml">` + "\n")
	for _, p := range publicPages {
		for _, locale := range i18n.Langs {
			b.WriteString("  <url><loc>" + html.EscapeString(localizedPublicURL(origin, p, locale.Code)) + "</loc>\n")
			for _, alternate := range i18n.Langs {
				b.WriteString(`    <xhtml:link rel="alternate" hreflang="` + alternate.Code + `" href="` +
					html.EscapeString(localizedPublicURL(origin, p, alternate.Code)) + `"/>` + "\n")
			}
			b.WriteString(`    <xhtml:link rel="alternate" hreflang="x-default" href="` +
				html.EscapeString(localizedPublicURL(origin, p, i18n.Default)) + `"/>` + "\n")
			b.WriteString("  </url>\n")
		}
	}
	b.WriteString("</urlset>\n")
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	_, _ = io.WriteString(w, b.String())
}

// handleRobots serves /robots.txt: crawl the public pages, skip everything
// behind auth (those redirect to /login anyway — this just keeps crawlers from
// wasting the crawl budget on them), and point at the sitemap.
func (s *Server) handleRobots(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, strings.Join([]string{
		"User-agent: *",
		"Allow: /",
		"Disallow: /dashboard",
		"Disallow: /projects",
		"Disallow: /admin",
		"Disallow: /auth",
		"Disallow: /login",
		"Disallow: /signup",
		"Disallow: /start",
		"Disallow: /verify",
		"",
		"Sitemap: " + s.origin(r) + "/sitemap.xml",
		"",
	}, "\n"))
}
