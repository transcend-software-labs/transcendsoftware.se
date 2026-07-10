package llm

import "testing"

func TestExtractSpec(t *testing.T) {
	plan := "## Summary\nA bakery site.\n\n## Pages\n- Home\n\n" +
		"```json\n{\"pages\":[{\"slug\":\"start\",\"paths\":[\"index\",\"home\"]," +
		"\"names\":{\"sv\":\"Startsidan\",\"en\":\"the home page\"},\"included\":\"Hero och kontakt\"}]," +
		"\"not_included\":[\"Onlinebetalning\"]," +
		"\"content_needed\":[{\"slug\":\"logo\",\"names\":{\"sv\":\"Logotyp\"},\"required\":true}]}\n```"

	spec, cleaned := ExtractSpec(plan)
	if len(spec.Pages) != 1 || spec.Pages[0].Slug != "start" {
		t.Fatalf("pages = %+v", spec.Pages)
	}
	if got := spec.Pages[0].Name("sv"); got != "Startsidan" {
		t.Errorf("sv name = %q", got)
	}
	if got := spec.Pages[0].Name("ru"); got != "the home page" {
		t.Errorf("ru name should fall back to en, got %q", got)
	}
	if len(spec.NotIncluded) != 1 || len(spec.ContentNeeded) != 1 {
		t.Errorf("scope/content = %+v / %+v", spec.NotIncluded, spec.ContentNeeded)
	}
	if contains(cleaned, "```") {
		t.Errorf("json block not stripped from plan:\n%s", cleaned)
	}
	if !contains(cleaned, "## Summary") {
		t.Errorf("prose lost from plan:\n%s", cleaned)
	}
}

func TestExtractSpecMissing(t *testing.T) {
	plan := "## Summary\nNo structured block here.\n## Pages\n- Home"
	spec, cleaned := ExtractSpec(plan)
	if len(spec.Pages) != 0 {
		t.Errorf("expected empty spec, got %+v", spec.Pages)
	}
	if cleaned != plan {
		t.Errorf("plan should be unchanged when no block present")
	}
}

func TestFakePlanCarriesSpec(t *testing.T) {
	res, _ := Fake{}.Plan(nil, "A website for a home bakery")
	spec, _ := ExtractSpec(res.Plan)
	if len(spec.Pages) == 0 {
		t.Fatal("fake plan must carry a parseable spec for dev-mode UI")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
