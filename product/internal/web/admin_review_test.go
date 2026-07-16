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
}
