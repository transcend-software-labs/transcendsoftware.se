package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// EmailNotifier sends notifications via a Resend-compatible API. Configured
// from EMAIL_API_KEY / EMAIL_FROM; when unset, no email notifier is registered
// and the hooks UI shows email as unavailable.
type EmailNotifier struct {
	apiKey string
	from   string
	http   *http.Client
}

// NewEmailNotifier returns an email notifier, or nil if unconfigured.
func NewEmailNotifier(apiKey, from string) *EmailNotifier {
	if apiKey == "" || from == "" {
		return nil
	}
	return &EmailNotifier{apiKey: apiKey, from: from, http: &http.Client{Timeout: 15 * time.Second}}
}

func (n *EmailNotifier) Notify(ctx context.Context, target string, e Event) error {
	subject := fmt.Sprintf("New %s entry on %s", e.Table, e.Site)
	var b strings.Builder
	fmt.Fprintf(&b, "A new row was added to \"%s\" on %s:\n\n", e.Table, e.Site)
	for _, f := range e.Fields {
		fmt.Fprintf(&b, "%s: %s\n", f.Name, f.Value)
	}
	b.WriteString("\n— sent automatically by your site")

	msg := map[string]any{
		"from":    n.from,
		"to":      []string{target},
		"subject": subject,
		"text":    b.String(),
	}
	if e.ReplyTo != "" {
		msg["reply_to"] = e.ReplyTo
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
	req.Header.Set("authorization", "Bearer "+n.apiKey)
	req.Header.Set("content-type", "application/json")
	resp, err := n.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("email %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}
