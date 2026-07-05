package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Resend sends email via the Resend API (https://resend.com/docs). Selected
// when RESEND_API_KEY is set; From must be a verified sender on the domain.
type Resend struct {
	apiKey string
	from   string
	http   *http.Client
}

// NewResend returns a Resend-backed notifier. from is the verified sender
// address, e.g. "Transcend Forge <hello@transcendsoftware.se>".
func NewResend(apiKey, from string) *Resend {
	return &Resend{
		apiKey: apiKey,
		from:   from,
		http:   &http.Client{Timeout: 15 * time.Second},
	}
}

func (r *Resend) Send(ctx context.Context, to, subject, body string) error {
	payload, err := json.Marshal(map[string]any{
		"from":    r.from,
		"to":      []string{to},
		"subject": subject,
		"text":    body,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.resend.com/emails", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("authorization", "Bearer "+r.apiKey)
	req.Header.Set("content-type", "application/json")

	resp, err := r.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("resend: status %d: %s", resp.StatusCode, string(raw))
	}
	return nil
}
