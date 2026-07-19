package store

import (
	"context"
	"testing"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/project"
	"github.com/transcend-software-labs/rasmus-ai/internal/user"
)

func TestMarketingFunnelAggregatesWithoutVisitorIdentifiers(t *testing.T) {
	runUserStores(t, func(t *testing.T, st Store) {
		ctx := context.Background()
		now := time.Date(2097, 4, 5, 12, 0, 0, 0, time.UTC)
		for _, kind := range []string{MarketingLandingView, MarketingLandingView, MarketingStart, MarketingSignupView} {
			if err := st.RecordMarketingEvent(ctx, MarketingEvent{
				Kind: kind, Source: "linkedin", Medium: "social", Campaign: "launch", OccurredAt: now,
			}); err != nil {
				t.Fatalf("record %s: %v", kind, err)
			}
		}
		approved := now
		u := &user.User{ID: "marketing-user", Email: "marketing-user@utest.example", Verified: true, ApprovedAt: &approved, CreatedAt: now}
		if err := st.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
		p := &project.Project{ID: "marketing-project", UserID: u.ID, Name: "Marketing test", PreviewURL: "https://preview.example", Paid: true, CreatedAt: now, UpdatedAt: now}
		if err := st.CreateProject(ctx, p); err != nil {
			t.Fatal(err)
		}

		got, err := st.MarketingFunnel(ctx, now.Add(-time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if got.LandingViews != 2 || got.Starts != 1 || got.SignupViews != 1 ||
			got.Signups != 1 || got.Briefs != 1 || got.Approved != 1 || got.Previews != 1 || got.Paid != 1 {
			t.Fatalf("unexpected funnel: %+v", got)
		}
		if len(got.Sources) != 1 || got.Sources[0].Source != "linkedin" || got.Sources[0].Campaign != "launch" {
			t.Fatalf("unexpected sources: %+v", got.Sources)
		}
	})
}
