package web

import (
	"testing"

	"github.com/transcend-software-labs/rasmus-ai/internal/project"
)

// TestReviewChecks: the operator verdict is clean only when the build is live,
// captured, design-audit clean, and the critic didn't flag polish.
func TestReviewChecks(t *testing.T) {
	clean := reviewItem{
		Project: &project.Project{PreviewURL: "https://x", Status: project.StatusAccepted, Critique: "SHIP"},
		Shots:   []reviewShot{{Path: "/"}},
	}
	if !clean.ReviewClean() {
		t.Errorf("clean build should pass every check: %+v", clean.ReviewChecks())
	}

	// Design findings + a POLISH critique → flagged for the operator's eyes.
	flagged := reviewItem{
		Project: &project.Project{
			PreviewURL: "https://x", Status: project.StatusAccepted,
			Critique: "POLISH — hero too plain", Findings: []project.Finding{{Name: "contrast"}},
		},
		Shots: []reviewShot{{Path: "/"}},
	}
	if flagged.ReviewClean() {
		t.Error("findings + POLISH critique must flag the build")
	}

	// No preview / no screenshots → flagged.
	if (reviewItem{Project: &project.Project{}}).ReviewClean() {
		t.Error("a build with no preview or screenshots must be flagged")
	}

	// An empty critique means "critic didn't run" — it must not block delivery.
	noCrit := reviewItem{
		Project: &project.Project{PreviewURL: "https://x", Status: project.StatusPreviewReady},
		Shots:   []reviewShot{{Path: "/"}},
	}
	if !noCrit.ReviewClean() {
		t.Errorf("empty critique should not block: %+v", noCrit.ReviewChecks())
	}

	// The code-review check: unpaid + not run → clean (not due yet); paid +
	// not run → flagged (due, hold delivery); FIX verdict → flagged; SHIP → clean.
	base := project.Project{PreviewURL: "https://x", Status: project.StatusAccepted, Critique: "SHIP"}
	shots := []reviewShot{{Path: "/"}}
	paidPending := base
	paidPending.Paid = true
	if (reviewItem{Project: &paidPending, Shots: shots}).ReviewClean() {
		t.Error("paid with no code review yet must be flagged")
	}
	fixed := paidPending
	fixed.CodeReview = "FIX\n\nSQL injection in contact form"
	if it := (reviewItem{Project: &fixed, Shots: shots}); it.ReviewClean() || it.CodeReviewClean() {
		t.Error("a FIX code review must flag the build")
	}
	shipped := paidPending
	shipped.CodeReview = "SHIP\n\nAll good."
	if it := (reviewItem{Project: &shipped, Shots: shots}); !it.ReviewClean() || !it.CodeReviewClean() {
		t.Errorf("a SHIP code review should pass: %+v", it.ReviewChecks())
	}
}
