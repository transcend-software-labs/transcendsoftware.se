package project

import (
	"strings"
	"testing"
)

func TestDesignOptionBriefIncludesStructuredDirection(t *testing.T) {
	d := DesignOption{
		Name: "Field notes", Description: "Documentary and direct",
		Palette:     []string{"#F4F0E8", "#171713", "#C64A2E", "#FFFFFF"},
		DisplayFont: "Fraunces", BodyFont: "Manrope", HeroLayout: "asymmetric",
		ImageStyle: "Natural side light and tactile close crops",
		Signature:  "A circular crop crossing the grid", Boldness: "bold",
	}
	brief := d.Brief()
	for _, want := range []string{"Field notes", "#C64A2E", "Fraunces", "asymmetric", "Natural side light", "circular crop", "bold"} {
		if !strings.Contains(brief, want) {
			t.Errorf("brief missing %q:\n%s", want, brief)
		}
	}
}

func TestHeroConceptSelectionBindsOneImageDirection(t *testing.T) {
	p := &Project{DesignOptions: []DesignOption{{Name: "Direction"}}}
	p.SetHeroConcepts([]HeroConcept{
		{ID: "concept-a", Name: "A", ImageDirection: "soft daylight"},
		{ID: "concept-b", Name: "B", ImageDirection: "hard flash"},
	})
	if got := p.ImageArtDirection(); got != "" {
		t.Fatalf("unchosen concepts must not leak an art direction, got %q", got)
	}
	selected, ok := p.SelectHeroConcept("concept-b")
	if !ok || selected.Name != "B" {
		t.Fatalf("selection failed: %+v, %v", selected, ok)
	}
	if got := p.ImageArtDirection(); got != "hard flash" {
		t.Fatalf("image direction = %q", got)
	}
	concepts := p.HeroConcepts()
	if concepts[0].Selected || !concepts[1].Selected {
		t.Fatalf("expected exactly concept B selected: %+v", concepts)
	}
}

func TestTimelineIncludesConceptGate(t *testing.T) {
	if len(TimelineSteps) != 7 || TimelineSteps[2] != "concept" {
		t.Fatalf("unexpected timeline: %v", TimelineSteps)
	}
	for status, want := range map[Status]int{
		StatusNeedsInput: 1, StatusConcepting: 2, StatusNeedsConcept: 2,
		StatusPlanning: 3, StatusBuilding: 4, StatusPreviewReady: 5, StatusDelivered: 6,
	} {
		if got := (&Project{Status: status}).TimelineStep(); got != want {
			t.Errorf("%s step = %d, want %d", status, got, want)
		}
	}
}
