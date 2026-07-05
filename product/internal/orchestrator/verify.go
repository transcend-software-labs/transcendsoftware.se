package orchestrator

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Verifier smoke-checks a deployed preview before the customer is told it is
// ready. "Preview ready" is a claim we verify, never assert: the agent runs
// `fly deploy` inside the sandbox, and a politely-failed deploy would otherwise
// hand the customer a dead link.
type Verifier interface {
	// Verify returns nil once the site at url serves a real response.
	Verify(ctx context.Context, url string) error
}

// NoopVerifier accepts everything. Dev mode only: fake builds produce preview
// URLs that don't exist, so there is nothing real to check.
type NoopVerifier struct{}

func (NoopVerifier) Verify(context.Context, string) error { return nil }

// HTTPVerifier polls the deployed site until it serves HTTP 200 with a
// non-empty body, allowing time for the app's machine to come up.
type HTTPVerifier struct {
	Window   time.Duration // total time to wait for the site (default 2m)
	Interval time.Duration // poll interval (default 3s)
	Client   *http.Client  // per-request client (default 10s timeout)
}

func (v HTTPVerifier) Verify(ctx context.Context, url string) error {
	window, interval, client := v.Window, v.Interval, v.Client
	if window == 0 {
		window = 2 * time.Minute
	}
	if interval == 0 {
		interval = 3 * time.Second
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}

	deadline := time.Now().Add(window)
	for {
		err := check(ctx, client, url)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the deployed site did not come up: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// check performs one probe: HTTP 200 with a non-empty body. (An nginx app with
// nothing deployed answers 403/404, so a bare status check is already telling.)
func check(ctx context.Context, client *http.Client, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	if len(body) == 0 {
		return fmt.Errorf("empty response body")
	}
	return nil
}
