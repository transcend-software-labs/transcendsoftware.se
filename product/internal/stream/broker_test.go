package stream

import "testing"

func TestBroker_HistoryReplayAndLive(t *testing.T) {
	b := NewBroker(10)

	// Published before anyone subscribes → available as history.
	b.Publish("p1", Event{Type: "log", Data: "first"})

	hist, ch, cancel := b.Subscribe("p1")
	defer cancel()
	if len(hist) != 1 || hist[0].Data != "first" {
		t.Fatalf("expected 1 history event, got %v", hist)
	}

	// Published after subscribe → delivered live.
	b.Publish("p1", Event{Type: "log", Data: "second"})
	select {
	case e := <-ch:
		if e.Data != "second" {
			t.Errorf("expected live event 'second', got %q", e.Data)
		}
	default:
		t.Error("expected a live event")
	}
}

func TestBroker_ResetClearsHistory(t *testing.T) {
	b := NewBroker(10)
	b.Publish("p1", Event{Type: "log", Data: "x"})
	b.Reset("p1")
	hist, _, cancel := b.Subscribe("p1")
	defer cancel()
	if len(hist) != 0 {
		t.Errorf("expected history cleared, got %v", hist)
	}
}

func TestBroker_IsolatesProjects(t *testing.T) {
	b := NewBroker(10)
	b.Publish("p1", Event{Type: "log", Data: "for-p1"})
	hist, _, cancel := b.Subscribe("p2")
	defer cancel()
	if len(hist) != 0 {
		t.Errorf("p2 should not see p1 events, got %v", hist)
	}
}
