package i18n

import "testing"

// TestCatalogParity is the enforcement half of "AI manages translations
// completely": any string added to en.json without matching keys in every
// other locale fails the build, so a page can never quietly ship half-English.
func TestCatalogParity(t *testing.T) {
	en := catalogs[Default]
	if len(en) == 0 {
		t.Fatal("english catalog is empty")
	}
	for _, l := range Langs {
		cat, ok := catalogs[l.Code]
		if !ok {
			t.Fatalf("no catalog file for declared language %q", l.Code)
		}
		for k := range en {
			if v, ok := cat[k]; !ok || v == "" {
				t.Errorf("%s.json: missing key %q", l.Code, k)
			}
		}
		for k := range cat {
			if _, ok := en[k]; !ok {
				t.Errorf("%s.json: key %q not in en.json (en is the source of truth)", l.Code, k)
			}
		}
	}
}

func TestFallback(t *testing.T) {
	if got := T("ru", "nav.dashboard"); got != "Проекты" {
		t.Errorf("ru nav.dashboard = %q", got)
	}
	if got := T("de", "nav.dashboard"); got != "Dashboard" {
		t.Errorf("unsupported lang should fall back to english, got %q", got)
	}
	if got := T("en", "no.such.key"); got != "no.such.key" {
		t.Errorf("missing key should fall back to the key, got %q", got)
	}
}

func TestFromAcceptLanguage(t *testing.T) {
	cases := map[string]string{
		"sv-SE,sv;q=0.9,en;q=0.8": "sv",
		"ru":                      "ru",
		"de-DE,fr;q=0.9":          "en",
		"":                        "en",
	}
	for header, want := range cases {
		if got := FromAcceptLanguage(header); got != want {
			t.Errorf("FromAcceptLanguage(%q) = %q, want %q", header, got, want)
		}
	}
}
