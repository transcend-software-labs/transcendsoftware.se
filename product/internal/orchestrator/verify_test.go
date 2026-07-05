package orchestrator

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPVerifier_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>hello</html>"))
	}))
	defer srv.Close()

	v := HTTPVerifier{Window: time.Second, Interval: 10 * time.Millisecond}
	if err := v.Verify(context.Background(), srv.URL); err != nil {
		t.Fatalf("expected verification to pass: %v", err)
	}
}

func TestHTTPVerifier_RetriesUntilUp(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("up"))
	}))
	defer srv.Close()

	v := HTTPVerifier{Window: time.Second, Interval: 10 * time.Millisecond}
	if err := v.Verify(context.Background(), srv.URL); err != nil {
		t.Fatalf("expected verification to pass once the site came up: %v", err)
	}
}

func TestHTTPVerifier_FailsWhenNeverUp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	defer srv.Close()

	v := HTTPVerifier{Window: 60 * time.Millisecond, Interval: 10 * time.Millisecond}
	if err := v.Verify(context.Background(), srv.URL); err == nil {
		t.Fatal("expected verification to fail for a site that never comes up")
	}
}
