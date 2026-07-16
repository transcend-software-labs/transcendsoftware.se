package web

// SEO baseline. Search engines and social previews need three things this app
// serves for free: correct <head> metadata (see layout.html), machine-readable
// structured data, and a sitemap/robots pair. The scaffolding lives here; the
// per-page words are yours — set View.Description on every page you add, and
// list the page in publicPages so it lands in the sitemap. See AGENTS.md.

import (
	"encoding/json"
	"html"
	"html/template"
	"io"
	"net/http"
	"strings"
)

// publicPages are the crawlable URLs listed in /sitemap.xml.
//
// ADD EVERY NEW PUBLIC PAGE HERE. Leave out anything private or pointless to
// index (/app, /admin, /login, /signup) — robots.txt disallows those too.
var publicPages = []string{"/"}

// absURL builds the absolute URL for path from the request. Canonical and Open
// Graph tags must be absolute, and the sitemap must list real URLs — so we
// derive the origin from the host we're actually served on (preview domain,
// custom domain, or localhost) rather than hardcoding one.
func absURL(r *http.Request, path string) string {
	scheme := "https"
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = p // behind Fly's proxy the app itself speaks plain http
	} else if strings.HasPrefix(r.Host, "localhost") || strings.HasPrefix(r.Host, "127.0.0.1") {
		scheme = "http"
	}
	return scheme + "://" + r.Host + path
}

// siteJSONLD is the site-level structured data every page carries: schema.org
// Organization, the baseline Google understands.
//
// If this site is a local business (bakery, salon, studio, restaurant…), UPGRADE
// it: set "@type" to the specific LocalBusiness subtype and add the customer's
// real "address" (PostalAddress), "telephone", "openingHoursSpecification" and
// "image". That's what earns rich results and Maps eligibility — it's the single
// biggest SEO win available here, and it's only correct if the details are real.
func (s *Server) siteJSONLD(r *http.Request) template.JS {
	b, err := json.Marshal(map[string]any{
		"@context": "https://schema.org",
		"@type":    "Organization",
		"name":     s.siteName,
		"url":      absURL(r, "/"),
	})
	if err != nil {
		return "" // never break a page over metadata
	}
	return template.JS(b) // marshaled by us, so safe to inject into <script>
}

// handleSitemap serves /sitemap.xml from publicPages.
func (s *Server) handleSitemap(w http.ResponseWriter, r *http.Request) {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` + "\n")
	for _, p := range publicPages {
		b.WriteString("  <url><loc>" + html.EscapeString(absURL(r, p)) + "</loc></url>\n")
	}
	b.WriteString("</urlset>\n")
	w.Header().Set("content-type", "application/xml; charset=utf-8")
	_, _ = io.WriteString(w, b.String())
}

// handleRobots serves /robots.txt: crawl the public site, skip the app/admin
// area, and point at the sitemap. (An unpaid preview is kept out of the index by
// Forge's proxy sending X-Robots-Tag, not by this file.)
func (s *Server) handleRobots(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("content-type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "User-agent: *\nAllow: /\nDisallow: /app\nDisallow: /admin\nDisallow: /login\nDisallow: /signup\n\nSitemap: "+absURL(r, "/sitemap.xml")+"\n")
}
