// Package oauth is a small OAuth2 authorization-code client for social login
// (Google now, LinkedIn ready). Each provider is just endpoints + how to read
// the account's email; the flow is identical, so adding a provider is config.
package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Provider describes one OAuth2 identity provider.
type Provider struct {
	Name        string // "google", "linkedin"
	Label       string // "Google" — shown on the button
	ClientID    string
	ClientKey   string
	AuthURL     string
	TokenURL    string
	UserInfoURL string
	Scopes      []string
}

// Registry holds the configured providers, keyed by Name.
type Registry struct {
	providers map[string]Provider
	client    *http.Client
}

// NewRegistry builds a registry from the providers that have credentials set.
func NewRegistry(providers ...Provider) *Registry {
	m := map[string]Provider{}
	for _, p := range providers {
		if p.ClientID != "" && p.ClientKey != "" {
			m[p.Name] = p
		}
	}
	return &Registry{providers: m, client: &http.Client{Timeout: 15 * time.Second}}
}

// Enabled returns the configured providers in a stable order for rendering.
func (r *Registry) Enabled() []Provider {
	var out []Provider
	for _, name := range []string{"google", "linkedin"} {
		if p, ok := r.providers[name]; ok {
			out = append(out, p)
		}
	}
	return out
}

// Get returns a configured provider by name.
func (r *Registry) Get(name string) (Provider, bool) {
	p, ok := r.providers[name]
	return p, ok
}

// AuthCodeURL builds the URL to send the user to, carrying an anti-CSRF state.
func (r *Registry) AuthCodeURL(p Provider, redirectURI, state string) string {
	q := url.Values{
		"client_id":     {p.ClientID},
		"redirect_uri":  {redirectURI},
		"response_type": {"code"},
		"scope":         {strings.Join(p.Scopes, " ")},
		"state":         {state},
		"access_type":   {"online"},
		"prompt":        {"select_account"},
	}
	return p.AuthURL + "?" + q.Encode()
}

// Email exchanges the authorization code and returns the account's email.
func (r *Registry) Email(ctx context.Context, p Provider, code, redirectURI string) (string, error) {
	token, err := r.exchange(ctx, p, code, redirectURI)
	if err != nil {
		return "", err
	}
	return r.fetchEmail(ctx, p, token)
}

func (r *Registry) exchange(ctx context.Context, p Provider, code, redirectURI string) (string, error) {
	form := url.Values{
		"client_id":     {p.ClientID},
		"client_secret": {p.ClientKey},
		"code":          {code},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {redirectURI},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	req.Header.Set("accept", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("oauth: token exchange %d: %s", resp.StatusCode, truncate(raw))
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(raw, &tok); err != nil || tok.AccessToken == "" {
		return "", fmt.Errorf("oauth: no access token")
	}
	return tok.AccessToken, nil
}

func (r *Registry) fetchEmail(ctx context.Context, p Provider, accessToken string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.UserInfoURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("accept", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("oauth: userinfo %d: %s", resp.StatusCode, truncate(raw))
	}
	// Google returns {email, email_verified, ...}. Keep it simple and tolerant.
	var info struct {
		Email string `json:"email"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		return "", err
	}
	if info.Email == "" {
		return "", fmt.Errorf("oauth: no email in profile")
	}
	return strings.ToLower(info.Email), nil
}

func truncate(b []byte) string {
	if len(b) > 200 {
		return string(b[:200])
	}
	return string(b)
}

// Google returns the Google provider config for the given credentials.
func Google(clientID, clientKey string) Provider {
	return Provider{
		Name: "google", Label: "Google", ClientID: clientID, ClientKey: clientKey,
		AuthURL:     "https://accounts.google.com/o/oauth2/v2/auth",
		TokenURL:    "https://oauth2.googleapis.com/token",
		UserInfoURL: "https://openidconnect.googleapis.com/v1/userinfo",
		Scopes:      []string{"openid", "email", "profile"},
	}
}

// LinkedIn returns the LinkedIn provider config (OpenID Connect).
func LinkedIn(clientID, clientKey string) Provider {
	return Provider{
		Name: "linkedin", Label: "LinkedIn", ClientID: clientID, ClientKey: clientKey,
		AuthURL:     "https://www.linkedin.com/oauth/v2/authorization",
		TokenURL:    "https://www.linkedin.com/oauth/v2/accessToken",
		UserInfoURL: "https://api.linkedin.com/v2/userinfo",
		Scopes:      []string{"openid", "email", "profile"},
	}
}
