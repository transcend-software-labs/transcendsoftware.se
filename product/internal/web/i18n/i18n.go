// Package i18n is the Forge UI's translation layer: embedded JSON catalogs,
// one file per language, English as the source of truth.
//
// Nobody maintains these by hand — the AI working on this repo does. The
// contract that keeps that safe:
//   - en.json is authoritative; every user-visible string in the customer UI
//     is a key in it.
//   - TestCatalogParity fails the build when any locale is missing (or has
//     extra) keys, so adding a string without adding its translations is a
//     red CI, not a silently-English page.
//   - At runtime a missing key falls back to English, then to the key itself —
//     users never see a hole.
//
// Adding a language = adding one JSON file + one entry in Langs.
package i18n

import (
	"embed"
	"encoding/json"
	"path"
	"strings"
)

//go:embed locales/*.json
var localesFS embed.FS

// Lang is one selectable UI language.
type Lang struct {
	Code  string // BCP-47-ish primary tag, doubles as the catalog filename
	Label string // native-language label for the selector
}

// Langs is the selector, in display order.
var Langs = []Lang{
	{"en", "English"},
	{"sv", "Svenska"},
	{"ru", "Русский"},
}

const Default = "en"

var catalogs = map[string]map[string]string{}

func init() {
	entries, err := localesFS.ReadDir("locales")
	if err != nil {
		panic(err)
	}
	for _, e := range entries {
		raw, err := localesFS.ReadFile(path.Join("locales", e.Name()))
		if err != nil {
			panic(err)
		}
		m := map[string]string{}
		if err := json.Unmarshal(raw, &m); err != nil {
			panic(e.Name() + ": " + err.Error())
		}
		catalogs[strings.TrimSuffix(e.Name(), ".json")] = m
	}
}

// Supported reports whether code is a selectable language.
func Supported(code string) bool {
	for _, l := range Langs {
		if l.Code == code {
			return true
		}
	}
	return false
}

// T translates key into lang, falling back to English and finally to the key
// itself — a missing translation must never break a page.
func T(lang, key string) string {
	if m, ok := catalogs[lang]; ok {
		if v, ok := m[key]; ok {
			return v
		}
	}
	if v, ok := catalogs[Default][key]; ok {
		return v
	}
	return key
}

// FromAcceptLanguage picks the best supported language from an Accept-Language
// header, defaulting to English. Deliberately simple: first supported primary
// tag in header order wins.
func FromAcceptLanguage(header string) string {
	for _, part := range strings.Split(header, ",") {
		tag := strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
		if i := strings.Index(tag, "-"); i > 0 {
			tag = tag[:i]
		}
		if Supported(strings.ToLower(tag)) {
			return strings.ToLower(tag)
		}
	}
	return Default
}
