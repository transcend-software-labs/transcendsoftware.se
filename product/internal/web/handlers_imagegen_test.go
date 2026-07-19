package web

import (
	"strings"
	"testing"

	"github.com/transcend-software-labs/rasmus-ai/internal/project"
)

func TestImagePromptsCarrySelectedSiteArtDirection(t *testing.T) {
	p := &project.Project{
		Name: "Solvarv", Brief: "A bakery", DesignBrief: "Warm editorial direction",
		DesignOptions: []project.DesignOption{{HeroConcepts: []project.HeroConcept{
			{ID: "concept-a", ImageDirection: "Soft north-window light, tight flour-dusted crops, restrained grain", Selected: true},
			{ID: "concept-b", ImageDirection: "An unchosen glossy studio look"},
		}}},
	}
	c := project.ContentItem{Slug: "hero", Names: map[string]string{"en": "Hero photo"}, Kind: "file", Generatable: true}
	prompt := defaultImagePrompt(p, c, "en")
	if !strings.Contains(prompt, "Shared image art direction for this entire site") || !strings.Contains(prompt, "Soft north-window light") {
		t.Fatalf("selected art direction missing from prompt: %s", prompt)
	}
	if strings.Contains(prompt, "unchosen glossy") {
		t.Fatalf("unchosen concept leaked into prompt: %s", prompt)
	}
}

func TestImageDesignContextDropsFullHeroCopy(t *testing.T) {
	brief := "Editorial direction\nPalette: #FFFFFF, #111111\n\nChosen hero concept: Statement\nHero copy: a long headline"
	got := imageDesignContext(brief)
	if got != "Editorial direction\nPalette: #FFFFFF, #111111" {
		t.Fatalf("image design context = %q", got)
	}
}

func TestImagePromptTruncationPreservesUTF8(t *testing.T) {
	if got := truncateImagePrompt("rågbröd", 4); got != "rågb" {
		t.Fatalf("rune-safe truncation = %q", got)
	}
}
