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
	lv := landingView{PriceStr: "299 kr", IncludedChanges: 3, OverageStr: "49 kr", DomainBuy: true}

	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "landing", View{Lang: "en", Data: lv}); err != nil {
		t.Fatalf("render landing: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, `class="examples"`) {
		t.Error("empty showcase must not render invented placeholders")
	}
	for _, want := range []string{
		"299 kr",                         // the live Stripe price
		"/month",                         // the per-interval suffix
		"3 changes a month",              // included allowance, %d-formatted
		"49 kr",                          // flat overage price, %s-formatted
		"Choose your domain here",        // the prominent domain callout
		"Search for and buy your domain", // the domain card
		"DNS and HTTPS automatically",    // auto-configured claim
		"yearly renewal price",           // price transparency
		"two DNS records",                // accurate BYOD setup expectation
	} {
		if !strings.Contains(out, want) {
			t.Errorf("landing pricing missing %q", want)
		}
	}
}

func TestLandingExamplesRenderRealResponsivePairs(t *testing.T) {
	tmpl, err := template.New("").Funcs(templateFuncs()).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		t.Fatalf("parse templates: %v", err)
	}
	examples := []landingExample{
		{Name: "North Bakery", Category: "Local business", Summary: "Ordering and opening hours made easy to find.",
			DesktopImage: "/static/examples/north-desktop.webp", MobileImage: "/static/examples/north-mobile.webp",
			DesktopAlt: "North Bakery homepage on desktop", MobileAlt: "North Bakery homepage on a phone", URL: "https://north.example"},
		{Name: "Harbor Studio", Category: "Creative studio", Summary: "A portfolio focused on recent work.", DesktopImage: "/static/examples/harbor-desktop.webp", DesktopAlt: "Harbor Studio portfolio"},
		{Name: "Linden Care", Category: "Professional service", Summary: "Services and contact options presented clearly.", DesktopImage: "/static/examples/linden-desktop.webp", DesktopAlt: "Linden Care homepage"},
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "landing", View{Lang: "en", StartURL: "/start", Data: landingView{Examples: examples}}); err != nil {
		t.Fatalf("render landing examples: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		`class="example-grid examples-3"`, `class="example-desktop"`, `class="example-mobile"`,
		`src="/static/examples/north-desktop.webp"`, `src="/static/examples/north-mobile.webp"`,
		`alt="North Bakery homepage on desktop"`, `loading="lazy"`, `href="https://north.example"`,
		"Real websites, built from a brief", "Visit the live site",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("landing showcase missing %q", want)
		}
	}
}

func TestLocalizedExampleTextFallsBackToEnglish(t *testing.T) {
	values := map[string]string{"en": "Bakery", "sv": "Bageri"}
	if got := localizedExampleText(values, "sv"); got != "Bageri" {
		t.Fatalf("Swedish example copy = %q", got)
	}
	if got := localizedExampleText(values, "ru"); got != "Bakery" {
		t.Fatalf("missing translation should use English, got %q", got)
	}
}

// TestLandingPricingFallback: when Stripe is unavailable (PriceStr == "") the
// page still shows a price via the copy fallback and never errors.
func TestLandingPricingFallback(t *testing.T) {
	tmpl, err := template.New("").Funcs(templateFuncs()).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		t.Fatalf("parse templates: %v", err)
	}
	lv := landingView{PriceStr: "", IncludedChanges: 1, OverageStr: "49 kr", DomainBuy: false}

	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "landing", View{Lang: "sv", Data: lv}); err != nil {
		t.Fatalf("render landing (fallback): %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "299 kr") { // sv fallback amount
		t.Errorf("expected fallback price; got:\n%s", out)
	}
	if strings.Contains(out, "Välj din domän här") { // domain marketing hidden when DomainBuy=false
		t.Errorf("domain marketing should be hidden when domain buying is unavailable")
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
