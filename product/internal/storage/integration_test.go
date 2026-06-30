package storage

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

// TestIntegration_PutPresignGet exercises the S3 path against a real
// S3-compatible backend (MinIO locally). It uploads an object, presigns a GET
// URL, downloads it, and checks the bytes round-trip. Skipped unless enabled:
//
//	STORAGE_SMOKE=1 STORAGE_ENDPOINT=localhost:9100 STORAGE_ACCESS_KEY=forge \
//	  STORAGE_SECRET_KEY=forge-secret STORAGE_BUCKET=forge-test \
//	  go test ./internal/storage/ -run Integration -v
func TestIntegration_PutPresignGet(t *testing.T) {
	if os.Getenv("STORAGE_SMOKE") == "" {
		t.Skip("set STORAGE_SMOKE=1 (+ STORAGE_* env) to run the live MinIO/S3 test")
	}
	s, err := NewS3(NewS3Params{
		Endpoint:  os.Getenv("STORAGE_ENDPOINT"),
		AccessKey: os.Getenv("STORAGE_ACCESS_KEY"),
		SecretKey: os.Getenv("STORAGE_SECRET_KEY"),
		Bucket:    os.Getenv("STORAGE_BUCKET"),
		Region:    "us-east-1",
		UseSSL:    false,
	})
	if err != nil {
		t.Fatalf("NewS3 (bucket ensure): %v", err)
	}

	ctx := context.Background()
	key := "test/hello.txt"
	content := []byte("hello forge")

	if err := s.Put(ctx, key, "text/plain", bytes.NewReader(content), int64(len(content))); err != nil {
		t.Fatalf("put: %v", err)
	}

	url, err := s.PresignGet(ctx, key, time.Minute)
	if err != nil {
		t.Fatalf("presign: %v", err)
	}
	t.Logf("presigned: %s", url)

	resp, err := http.Get(url) //nolint:gosec // presigned URL from our own backend
	if err != nil {
		t.Fatalf("get presigned: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(body, content) {
		t.Errorf("round-trip mismatch: got %q want %q (status %d)", body, content, resp.StatusCode)
	}
}
