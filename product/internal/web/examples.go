package web

import "github.com/transcend-software-labs/rasmus-ai/internal/web/i18n"

// showcaseExample is one real Forge-built site. Keep this source list empty
// until the owner has permission to publish the work and supplies the two
// screenshots. The landing section is data-gated, so an empty portfolio never
// renders placeholder or invented social proof in production.
type showcaseExample struct {
	Name         map[string]string
	Category     map[string]string
	Summary      map[string]string
	DesktopAlt   map[string]string
	MobileAlt    map[string]string
	DesktopImage string // /static/examples/<slug>-desktop.webp (1440×900)
	MobileImage  string // /static/examples/<slug>-mobile.webp (390×844); optional
	URL          string // optional public customer URL
	AIImages     bool   // disclose that the example's image world was generated in Forge
}

// publishedShowcase is intentionally empty until real work is supplied.
// Adding an example here is the only code change needed after its optimized
// images are placed under internal/web/static/examples/.
var publishedShowcase = []showcaseExample{}

// landingExample is the already-localized template shape.
type landingExample struct {
	Name         string
	Category     string
	Summary      string
	DesktopAlt   string
	MobileAlt    string
	DesktopImage string
	MobileImage  string
	URL          string
	AIImages     bool
}

func localizedExampleText(values map[string]string, lang string) string {
	if value := values[lang]; value != "" {
		return value
	}
	return values[i18n.Default]
}

func landingExamples(lang string) []landingExample {
	out := make([]landingExample, 0, len(publishedShowcase))
	for _, example := range publishedShowcase {
		out = append(out, landingExample{
			Name: localizedExampleText(example.Name, lang), Category: localizedExampleText(example.Category, lang),
			Summary: localizedExampleText(example.Summary, lang), DesktopAlt: localizedExampleText(example.DesktopAlt, lang),
			MobileAlt: localizedExampleText(example.MobileAlt, lang), DesktopImage: example.DesktopImage,
			MobileImage: example.MobileImage, URL: example.URL, AIImages: example.AIImages,
		})
	}
	return out
}
