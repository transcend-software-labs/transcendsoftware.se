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
	apiKey  string
	from    string
	replyTo string
	http    *http.Client
}

// NewResend returns a Resend-backed notifier. from is the verified sender
// address, e.g. "Transcend Forge <hello@forge.transcendsoftware.se>". replyTo
// (optional) is a monitored inbox — set it when the sending domain can't
// receive mail, so replies still reach a real address instead of bouncing.
// Avoid "noreply@" senders: they signal one-way mail and hurt deliverability.
func NewResend(apiKey, from, replyTo string) *Resend {
	return &Resend{
		apiKey:  apiKey,
		from:    from,
		replyTo: replyTo,
		http:    &http.Client{Timeout: 15 * time.Second},
	}
}

func (r *Resend) Send(ctx context.Context, to, subject, body string) error {
	msg := map[string]any{
		"from":    r.from,
		"to":      []string{to},
		"subject": subject,
		"text":    body,
	}
	if r.replyTo != "" {
		msg["reply_to"] = r.replyTo
	}
	payload, err := json.Marshal(msg)
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
