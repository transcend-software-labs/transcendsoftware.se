package project

import (
	"testing"
	"time"
)

// TestChangeAllowance covers the monthly change-allowance math: counting within
// a window, the flat overage past it, and the reset when the window rolls into
// the next month.
func TestChangeAllowance(t *testing.T) {
	base := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	const perMonth = 3
	p := &Project{}

	// Three changes in the first window: all included, none overage.
	for i := 1; i <= perMonth; i++ {
		if over := p.RecordChange(base, perMonth); over {
			t.Fatalf("change %d should be included, got overage", i)
		}
	}
	if got := p.ChangesLeft(base, perMonth); got != 0 {
		t.Fatalf("allowance should be spent, ChangesLeft=%d", got)
	}
	// The fourth change in the same window is overage.
	if over := p.RecordChange(base, perMonth); !over {
		t.Fatal("fourth change in the same month should be overage")
	}
	if p.ChangesThisPeriod != 4 {
		t.Fatalf("ChangesThisPeriod=%d, want 4", p.ChangesThisPeriod)
	}

	// A month later the window rolls: allowance resets before any new change.
	next := base.AddDate(0, 1, 1)
	if got := p.ChangesLeft(next, perMonth); got != perMonth {
		t.Fatalf("ChangesLeft after rollover=%d, want %d", got, perMonth)
	}
	if over := p.RecordChange(next, perMonth); over {
		t.Fatal("first change in the new month should be included, not overage")
	}
	if p.ChangesThisPeriod != 1 {
		t.Fatalf("counter should reset to 1 in the new window, got %d", p.ChangesThisPeriod)
	}
	if !p.ChangePeriodStart.After(base) {
		t.Fatal("ChangePeriodStart should have advanced into the new window")
	}
}

// TestCanRequestChange gates the paid monthly change path to a paid site in a
// settled, live state.
func TestCanRequestChange(t *testing.T) {
	cases := []struct {
		name string
		paid bool
		st   Status
		want bool
	}{
		{"paid preview_ready", true, StatusPreviewReady, true},
		{"paid delivered", true, StatusDelivered, true},
		{"paid building", true, StatusBuilding, false},
		{"unpaid preview_ready", false, StatusPreviewReady, false},
		{"unpaid delivered", false, StatusDelivered, false},
	}
	for _, c := range cases {
		p := &Project{Paid: c.paid, Status: c.st}
		if got := p.CanRequestChange(); got != c.want {
			t.Errorf("%s: CanRequestChange()=%v, want %v", c.name, got, c.want)
		}
	}
}
