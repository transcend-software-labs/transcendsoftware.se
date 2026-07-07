package hooks

import (
	"context"
	"strings"
	"testing"
)

func TestGuardAddress_BlocksInternalAllowsPublic(t *testing.T) {
	blocked := []string{
		"127.0.0.1:80", "10.0.0.5:443", "192.168.1.1:80", "172.16.0.1:80",
		"169.254.169.254:80", "[::1]:80", "[fdaa:0:1::3]:80", "[fe80::1]:80", "0.0.0.0:80",
	}
	for _, a := range blocked {
		if err := guardAddress(a); err == nil {
			t.Errorf("expected %s to be blocked", a)
		}
	}
	for _, a := range []string{"8.8.8.8:443", "1.1.1.1:80", "[2606:4700:4700::1111]:443"} {
		if err := guardAddress(a); err != nil {
			t.Errorf("expected %s to be allowed, got %v", a, err)
		}
	}
}

func TestValidTargetURL(t *testing.T) {
	ok := []string{"https://hooks.slack.com/services/x/y/z", "https://example.com/wh"}
	bad := []string{"", "ftp://x", "notaurl", "https://x y.com", "javascript:alert(1)"}
	for _, u := range ok {
		if !ValidTargetURL(u) {
			t.Errorf("%q should be valid", u)
		}
	}
	for _, u := range bad {
		if ValidTargetURL(u) {
			t.Errorf("%q should be invalid", u)
		}
	}
}

func TestNotifiers_RefuseInternalTargets(t *testing.T) {
	e := Event{Site: "S", Table: "messages", Fields: []Field{{Name: "name", Value: "Anna"}}}
	for _, target := range []string{"http://127.0.0.1:8080/x", "http://10.0.0.1/x", "http://[fdaa::1]/x"} {
		if err := (SlackNotifier{}).Notify(context.Background(), target, e); err == nil {
			t.Errorf("slack should refuse internal target %s", target)
		}
		if err := (WebhookNotifier{}).Notify(context.Background(), target, e); err == nil {
			t.Errorf("webhook should refuse internal target %s", target)
		}
	}
}

func TestPayloadFormats(t *testing.T) {
	e := Event{Site: "Lugn Yoga", Table: "messages", Fields: []Field{{Name: "name", Value: "Sofia"}, {Name: "msg", Value: "hej"}}}
	if s := slackText(e); !strings.Contains(s, "Lugn Yoga") || !strings.Contains(s, "Sofia") || !strings.Contains(s, "messages") {
		t.Errorf("slack text missing content: %q", s)
	}
	b := webhookBody(e)
	if b["site"] != "Lugn Yoga" || b["table"] != "messages" {
		t.Errorf("webhook body wrong: %+v", b)
	}
	if data, ok := b["data"].(map[string]string); !ok || data["name"] != "Sofia" {
		t.Errorf("webhook data wrong: %+v", b["data"])
	}
}
