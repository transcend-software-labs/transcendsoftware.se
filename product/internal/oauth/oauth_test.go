package oauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRegistry_OnlyConfiguredEnabled(t *testing.T) {
	r := NewRegistry(
		Google("gid", "gsec"),
		LinkedIn("", ""), // no creds → hidden
	)
	en := r.Enabled()
	if len(en) != 1 || en[0].Name != "google" {
		t.Fatalf("expected only google enabled, got %+v", en)
	}
	if _, ok := r.Get("linkedin"); ok {
		t.Error("linkedin should not be configured")
	}
}

func TestAuthCodeURL(t *testing.T) {
	r := NewRegistry(Google("gid", "gsec"))
	p, _ := r.Get("google")
	u := r.AuthCodeURL(p, "https://app.example/auth/google/callback", "st8")
	for _, want := range []string{"client_id=gid", "state=st8", "response_type=code", "scope=openid"} {
		if !strings.Contains(u, want) {
			t.Errorf("auth url missing %q: %s", want, u)
		}
	}
}

func TestEmail_ExchangeAndFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			_, _ = w.Write([]byte(`{"access_token":"at123"}`))
		case "/userinfo":
			if r.Header.Get("Authorization") != "Bearer at123" {
				t.Errorf("missing bearer token")
			}
			_, _ = w.Write([]byte(`{"email":"Person@Example.com","email_verified":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	r := NewRegistry(Provider{
		Name: "google", Label: "Google", ClientID: "id", ClientKey: "sec",
		TokenURL: srv.URL + "/token", UserInfoURL: srv.URL + "/userinfo",
	})
	p, _ := r.Get("google")
	email, err := r.Email(context.Background(), p, "code123", "https://app.example/cb")
	if err != nil {
		t.Fatalf("email: %v", err)
	}
	if email != "person@example.com" { // lowercased
		t.Fatalf("unexpected email %q", email)
	}
}
