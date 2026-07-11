package imagegen

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGenerate(t *testing.T) {
	png := []byte("\x89PNG fake")
	var gotPath, gotAuth, gotModel string
	var gotN float64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("authorization")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel, _ = body["model"].(string)
		gotN, _ = body["n"].(float64)
		b64 := base64.StdEncoding.EncodeToString(png)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{
			map[string]any{"b64_json": b64}, map[string]any{"b64_json": b64}, map[string]any{"b64_json": b64},
		}})
	}))
	defer srv.Close()

	c := New(srv.URL, "sk-test", "gpt-image-2")
	imgs, err := c.Generate(context.Background(), "a logo", 3)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(imgs) != 3 || string(imgs[0]) != string(png) {
		t.Errorf("got %d images", len(imgs))
	}
	if gotPath != "/images/generations" || gotModel != "gpt-image-2" || gotN != 3 {
		t.Errorf("path=%s model=%s n=%v", gotPath, gotModel, gotN)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("auth = %q", gotAuth)
	}
}

func TestEditSendsMultipartImage(t *testing.T) {
	var ct string
	var hadImage bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ct = r.Header.Get("content-type")
		_ = r.ParseMultipartForm(1 << 20)
		if r.MultipartForm != nil {
			_, hadImage = r.MultipartForm.File["image"]
		}
		b64 := base64.StdEncoding.EncodeToString([]byte("edited"))
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"b64_json": b64}}})
	}))
	defer srv.Close()

	c := New(srv.URL, "sk-test", "gpt-image-2")
	imgs, err := c.Edit(context.Background(), []byte("original png"), "make it minimal", 1)
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if len(imgs) != 1 || string(imgs[0]) != "edited" {
		t.Errorf("got %v", imgs)
	}
	if !strings.HasPrefix(ct, "multipart/form-data") {
		t.Errorf("content-type = %q", ct)
	}
	if !hadImage {
		t.Error("edit must upload the reference image")
	}
}

func TestErrorSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "bad prompt", "type": "invalid_request"}})
	}))
	defer srv.Close()
	c := New(srv.URL, "sk-test", "gpt-image-2")
	if _, err := c.Generate(context.Background(), "x", 3); err == nil || !strings.Contains(err.Error(), "bad prompt") {
		t.Errorf("expected surfaced error, got %v", err)
	}
}
