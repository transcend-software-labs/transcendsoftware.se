package web

import (
	"bytes"
	"html/template"
	"strings"
	"testing"
)

// TestLandingPricing renders the public landing page with the pricing block
// populated — the price, the included-changes allowance, the flat overage and
// the domain card must all show, in whichever locale the visitor uses.
func TestLandingPricing(t *testing.T) {
	tmpl, err := template.New("").Funcs(templateFuncs()).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		t.Fatalf("parse templates: %v", err)
	}
	lv := landingView{PriceStr: "299 kr", IncludedChanges: 3, OverageStr: "49 kr", Domains: true}

	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "landing", View{Lang: "en", Data: lv}); err != nil {
		t.Fatalf("render landing: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"299 kr",                  // the live Stripe price
		"/month",                  // the per-interval suffix
		"3 changes a month",       // included allowance, %d-formatted
		"49 kr",                   // flat overage price, %s-formatted
		"Buy a domain right here", // the domain card
		"DNS and HTTPS",           // auto-configured claim
	} {
		if !strings.Contains(out, want) {
			t.Errorf("landing pricing missing %q", want)
		}
	}
}

// TestLandingPricingFallback: when Stripe is unavailable (PriceStr == "") the
// page still shows a price via the copy fallback and never errors.
func TestLandingPricingFallback(t *testing.T) {
	tmpl, err := template.New("").Funcs(templateFuncs()).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		t.Fatalf("parse templates: %v", err)
	}
	lv := landingView{PriceStr: "", IncludedChanges: 1, OverageStr: "49 kr", Domains: false}

	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "landing", View{Lang: "sv", Data: lv}); err != nil {
		t.Fatalf("render landing (fallback): %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "299 kr") { // sv fallback amount
		t.Errorf("expected fallback price; got:\n%s", out)
	}
	if strings.Contains(out, "konfigurerar Forge") { // domain card hidden when Domains=false
		t.Errorf("domain card should be hidden when Domains is false")
	}
	// Singular allowance copy for IncludedChanges == 1.
	if !strings.Contains(out, "1 ändring i månaden") {
		t.Errorf("expected singular changes copy; got:\n%s", out)
	}
}

// TestLandingNilData: the pricing section is guarded by {{with .Data}}, so a
// render with no data (Data == nil) must not error.
func TestLandingNilData(t *testing.T) {
	tmpl, err := template.New("").Funcs(templateFuncs()).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		t.Fatalf("parse templates: %v", err)
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "landing", View{Lang: "en", Data: nil}); err != nil {
		t.Fatalf("render landing with nil Data must not error: %v", err)
	}
	if strings.Contains(buf.String(), "class=\"pricing\"") {
		t.Errorf("pricing section should be skipped when Data is nil")
	}
}
