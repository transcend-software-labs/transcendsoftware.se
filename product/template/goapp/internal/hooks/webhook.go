package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"
)

// Slack and generic-webhook notifiers POST to an owner-supplied URL. That URL is
// UNTRUSTED, and this app runs on a private network (Fly 6PN), so a naive POST
// would be an SSRF hole — a hook could probe internal services. safeClient's
// dialer validates the *resolved* IP of every connection (including redirects
// and DNS-rebinding) and refuses anything that isn't a public address.

var safeClient = &http.Client{
	Timeout: 15 * time.Second,
	Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout: 10 * time.Second,
			Control: func(_, address string, _ syscall.RawConn) error { return guardAddress(address) },
		}).DialContext,
	},
}

// guardAddress rejects connections to non-public IPs (loopback, RFC1918, ULA /
// Fly 6PN fdaa::/16, link-local, unspecified, multicast).
func guardAddress(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("blocked: unresolved address %q", address)
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return fmt.Errorf("blocked internal address %s", ip)
	}
	return nil
}

// ValidTargetURL reports whether a hook target is a syntactically acceptable
// http(s) URL (the IP-level SSRF check happens at send time in safeClient).
func ValidTargetURL(raw string) bool {
	raw = strings.TrimSpace(raw)
	return (strings.HasPrefix(raw, "https://") || strings.HasPrefix(raw, "http://")) &&
		len(raw) > 10 && len(raw) <= 2000 && !strings.ContainsAny(raw, " \n\r\t")
}

func postJSON(ctx context.Context, url string, payload any) error {
	if !ValidTargetURL(url) {
		return fmt.Errorf("invalid target url")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	resp, err := safeClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("%d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}

// SlackNotifier posts to a Slack incoming-webhook URL.
type SlackNotifier struct{}

func (SlackNotifier) Notify(ctx context.Context, target string, e Event) error {
	return postJSON(ctx, target, map[string]string{"text": slackText(e)})
}

func slackText(e Event) string {
	var b strings.Builder
	fmt.Fprintf(&b, "*New %s entry on %s*\n", e.Table, e.Site)
	for _, f := range e.Fields {
		fmt.Fprintf(&b, "• %s: %s\n", f.Name, f.Value)
	}
	return b.String()
}

// WebhookNotifier POSTs the row as JSON to any URL (Zapier/Make/n8n/custom).
type WebhookNotifier struct{}

func (WebhookNotifier) Notify(ctx context.Context, target string, e Event) error {
	return postJSON(ctx, target, webhookBody(e))
}

func webhookBody(e Event) map[string]any {
	data := make(map[string]string, len(e.Fields))
	for _, f := range e.Fields {
		data[f.Name] = f.Value
	}
	return map[string]any{"site": e.Site, "table": e.Table, "data": data}
}
