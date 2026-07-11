package project

import "testing"

func TestContentItemIsFile(t *testing.T) {
	cases := []struct {
		name string
		item ContentItem
		want bool
	}{
		{"explicit file", ContentItem{Kind: "file", Names: map[string]string{"en": "Contact email"}}, true},
		{"explicit text", ContentItem{Kind: "text", Names: map[string]string{"en": "Logo"}}, false},
		{"infer logo", ContentItem{Slug: "logo", Names: map[string]string{"en": "Logo"}}, true},
		{"infer photos", ContentItem{Slug: "photos", Names: map[string]string{"sv": "Bilder"}}, true},
		{"infer hero image", ContentItem{Slug: "hero", Names: map[string]string{"en": "Hero image"}}, true},
		{"infer email is text", ContentItem{Slug: "contact_email", Names: map[string]string{"en": "Contact email"}}, false},
		{"infer copy is text", ContentItem{Slug: "about_copy", Names: map[string]string{"en": "About text"}}, false},
		{"infer route list is text", ContentItem{Slug: "routes", Names: map[string]string{"en": "Route list and prices"}}, false},
	}
	for _, tc := range cases {
		if got := tc.item.IsFile(); got != tc.want {
			t.Errorf("%s: IsFile() = %v, want %v", tc.name, got, tc.want)
		}
	}
}
