package fly

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeGraphQL serves the org-id query and the token mutation.
func fakeGraphQL(t *testing.T, mintFails bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		q := string(body)
		w.Header().Set("content-type", "application/json")
		switch {
		case strings.Contains(q, "organization(slug"):
			_, _ = w.Write([]byte(`{"data":{"organization":{"id":"org_node_123"}}}`))
		case strings.Contains(q, "createLimitedAccessToken"):
			if mintFails {
				_, _ = w.Write([]byte(`{"errors":[{"message":"Not authorized to access this createlimitedaccesstoken"}]}`))
				return
			}
			// Assert the mutation carries the app-scoped params.
			if !strings.Contains(q, `"profile":"deploy"`) || !strings.Contains(q, `"app_id":"forge-abc"`) {
				t.Errorf("mutation missing deploy/app scope: %s", q)
			}
			_, _ = w.Write([]byte(`{"data":{"createLimitedAccessToken":{"limitedAccessToken":{"tokenHeader":"FlyV1 fm2_scopedtoken"}}}}`))
		default:
			t.Errorf("unexpected graphql query: %s", q)
		}
	}))
}

func newTokenClient(gqlURL, fallback string) *HTTP {
	return NewHTTP(Options{
		Token: "runtime-token", Org: "acme", DeployToken: fallback,
		GraphQLURL: gqlURL, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func TestAppDeployToken_MintsScoped(t *testing.T) {
	srv := fakeGraphQL(t, false)
	defer srv.Close()
	h := newTokenClient(srv.URL, "org-scoped-fallback")

	tok, err := h.AppDeployToken(context.Background(), "forge-abc")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if tok != "FlyV1 fm2_scopedtoken" {
		t.Fatalf("expected the minted scoped token, got %q", tok)
	}
}

func TestAppDeployToken_FallsBackWhenMintFails(t *testing.T) {
	srv := fakeGraphQL(t, true)
	defer srv.Close()
	h := newTokenClient(srv.URL, "org-scoped-fallback")

	tok, err := h.AppDeployToken(context.Background(), "forge-abc")
	if err != nil {
		t.Fatalf("expected fallback, got error: %v", err)
	}
	if tok != "org-scoped-fallback" {
		t.Fatalf("expected the configured fallback token, got %q", tok)
	}
}

func TestAppDeployToken_ErrorsWhenNoFallback(t *testing.T) {
	srv := fakeGraphQL(t, true)
	defer srv.Close()
	h := newTokenClient(srv.URL, "") // no fallback configured

	if _, err := h.AppDeployToken(context.Background(), "forge-abc"); err == nil {
		t.Fatal("expected an error when minting fails and no fallback is set")
	}
}

func TestOrgID_Cached(t *testing.T) {
	var orgQueries int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "organization(slug") {
			orgQueries++
		}
		w.Header().Set("content-type", "application/json")
		if strings.Contains(string(body), "organization(slug") {
			_, _ = w.Write([]byte(`{"data":{"organization":{"id":"org_node_123"}}}`))
		} else {
			_, _ = w.Write([]byte(`{"data":{"createLimitedAccessToken":{"limitedAccessToken":{"tokenHeader":"FlyV1 t"}}}}`))
		}
	}))
	defer srv.Close()
	h := newTokenClient(srv.URL, "")

	for i := 0; i < 3; i++ {
		if _, err := h.AppDeployToken(context.Background(), "forge-abc"); err != nil {
			t.Fatalf("mint %d: %v", i, err)
		}
	}
	if orgQueries != 1 {
		t.Fatalf("org id should be resolved once and cached, saw %d queries", orgQueries)
	}
}

// guard against accidental JSON shape drift in the mutation variables.
func TestMintVariablesShape(t *testing.T) {
	var captured string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "createLimitedAccessToken") {
			captured = string(body)
		}
		w.Header().Set("content-type", "application/json")
		if strings.Contains(string(body), "organization(slug") {
			_, _ = w.Write([]byte(`{"data":{"organization":{"id":"org_node_123"}}}`))
		} else {
			_, _ = w.Write([]byte(`{"data":{"createLimitedAccessToken":{"limitedAccessToken":{"tokenHeader":"FlyV1 t"}}}}`))
		}
	}))
	defer srv.Close()
	h := newTokenClient(srv.URL, "")
	if _, err := h.AppDeployToken(context.Background(), "forge-abc"); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Variables struct {
			Input map[string]any `json:"input"`
		} `json:"variables"`
	}
	if err := json.Unmarshal([]byte(captured), &payload); err != nil {
		t.Fatalf("decode captured: %v", err)
	}
	in := payload.Variables.Input
	if in["organizationId"] != "org_node_123" || in["profile"] != "deploy" {
		t.Errorf("bad mutation input: %+v", in)
	}
	if pp, ok := in["profileParams"].(map[string]any); !ok || pp["app_id"] != "forge-abc" {
		t.Errorf("bad profileParams: %+v", in["profileParams"])
	}
}
