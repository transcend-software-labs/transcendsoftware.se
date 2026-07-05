// Command templatepush tars a starter-template directory and uploads it to
// object storage, where the orchestrator presigns it into first builds.
//
// Usage (STORAGE_* env must be set, same vars as the server):
//
//	go run ./cmd/templatepush -dir template/goapp -key templates/goapp.tgz
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/config"
	"github.com/transcend-software-labs/rasmus-ai/internal/storage"
)

// skipped are local artifacts that must not ship in the template.
var skipped = map[string]bool{".git": true, "data": true, "bin": true, "node_modules": true}

func main() {
	dir := flag.String("dir", "template/goapp", "template directory to upload")
	key := flag.String("key", "templates/goapp.tgz", "object-storage key to upload to")
	flag.Parse()

	cfg := config.Load()
	if !cfg.StorageEnabled() {
		fatal(fmt.Errorf("STORAGE_ENDPOINT/ACCESS_KEY/SECRET_KEY must be set"))
	}
	store, err := storage.NewS3(storage.NewS3Params{
		Endpoint:  cfg.StorageEndpoint,
		AccessKey: cfg.StorageAccessKey,
		SecretKey: cfg.StorageSecretKey,
		Bucket:    cfg.StorageBucket,
		Region:    cfg.StorageRegion,
		UseSSL:    cfg.StorageUseSSL,
	})
	if err != nil {
		fatal(err)
	}

	tarball, files, err := tarDir(*dir)
	if err != nil {
		fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := store.Put(ctx, *key, "application/gzip",
		bytes.NewReader(tarball), int64(len(tarball))); err != nil {
		fatal(err)
	}
	fmt.Printf("uploaded %s → %s (%d files, %.1f KB)\n", *dir, *key, files, float64(len(tarball))/1024)
}

// tarDir packs dir into a gzipped tar whose paths are relative to dir's root
// (matching how snapshots are packed: `tar -czf - -C dir .`).
func tarDir(dir string) ([]byte, int, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	files := 0

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		// Skip local artifacts at any depth.
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			if skipped[part] {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if d.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if _, err := tw.Write(b); err != nil {
			return err
		}
		files++
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	if err := tw.Close(); err != nil {
		return nil, 0, err
	}
	if err := gz.Close(); err != nil {
		return nil, 0, err
	}
	return buf.Bytes(), files, nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "templatepush:", err)
	os.Exit(1)
}
