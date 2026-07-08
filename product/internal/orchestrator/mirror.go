package orchestrator

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/transcend-software-labs/rasmus-ai/internal/builder"
	"github.com/transcend-software-labs/rasmus-ai/internal/github"
	"github.com/transcend-software-labs/rasmus-ai/internal/project"
)

// maxMirrorFile caps a single file mirrored to GitHub (skip stray large blobs).
const maxMirrorFile = 8 << 20 // 8 MB

// SetMirror enables GitHub mirroring of each project's source. Nil disables it.
func (o *Orchestrator) SetMirror(m github.Mirror) { o.mirror = m }

// mirrorToGitHub pushes the given workspace snapshot to the project's private
// repo (source review + a deploy-on-push Action) and persists p.RepoURL on
// success. It returns an error so callers can decide how to treat a failure:
// the build flow logs it and moves on (best-effort — never blocks a build),
// while RemirrorProject surfaces it to the admin.
func (o *Orchestrator) mirrorToGitHub(ctx context.Context, p *project.Project, snapshotKey, message string) error {
	if o.mirror == nil {
		return fmt.Errorf("github mirroring is not configured")
	}
	if snapshotKey == "" {
		return fmt.Errorf("no workspace snapshot to mirror")
	}
	raw, err := o.storage.Get(ctx, snapshotKey)
	if err != nil {
		return fmt.Errorf("read snapshot: %w", err)
	}
	files, err := untarGz(raw)
	if err != nil {
		return fmt.Errorf("untar snapshot: %w", err)
	}
	if len(files) == 0 {
		return fmt.Errorf("snapshot is empty")
	}

	appName := builder.DeployAppName(p.ID)
	files[".github/workflows/deploy.yml"] = []byte(deployWorkflow(appName))

	// The CI deploy token is longer-lived than the build token and lives only
	// as the repo's encrypted FLY_API_TOKEN secret. Non-fatal: without it the
	// mirror still lands, the deploy Action just can't authenticate yet.
	ciToken, err := o.machines.RepoDeployToken(ctx, appName)
	if err != nil {
		o.log.Warn("mirror: ci token unavailable; FLY_API_TOKEN will be unset", "project", p.ID, "err", err)
	}

	url, err := o.mirror.Push(ctx, github.PushSpec{
		Repo: appName, Message: message, Files: files, FlyToken: ciToken,
	})
	if err != nil {
		return fmt.Errorf("push: %w", err)
	}
	p.RepoURL = url
	if err := o.save(ctx, p); err != nil {
		return fmt.Errorf("save repo url: %w", err)
	}
	return nil
}

// RemirrorProject re-pushes a project's last successful workspace snapshot to
// its GitHub repo. It backfills projects built before mirroring worked and
// retries after a token-scope fix — no rebuild, no build-slot. Unlike the
// build-time mirror it surfaces the error so the admin sees the outcome.
func (o *Orchestrator) RemirrorProject(ctx context.Context, projectID string) error {
	p, err := o.store.ProjectByID(ctx, projectID)
	if err != nil {
		return err
	}
	if p.SnapshotKey == "" {
		return fmt.Errorf("no workspace snapshot yet — build a preview first")
	}
	return o.mirrorToGitHub(ctx, p, p.SnapshotKey, "Mirror: "+p.Name)
}

// untarGz reads a gzipped tar into path→content, skipping directories, .git and
// oversized files. Paths are normalized relative to the archive root.
func untarGz(raw []byte) (map[string][]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	files := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		name := strings.TrimPrefix(hdr.Name, "./")
		if name == "" || strings.HasPrefix(name, ".git/") || strings.Contains(name, "..") {
			continue
		}
		if hdr.Size > maxMirrorFile {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(tr, maxMirrorFile))
		if err != nil {
			return nil, err
		}
		files[name] = b
	}
	return files, nil
}

// deployWorkflow is the per-project GitHub Action: deploy to Fly on push to main.
func deployWorkflow(appName string) string {
	return fmt.Sprintf(`name: Deploy to Fly
on:
  push:
    branches: [main]
concurrency:
  group: deploy-%[1]s
  cancel-in-progress: true
jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: superfly/flyctl-actions/setup-flyctl@master
      - run: flyctl deploy --remote-only --app %[1]s
        env:
          FLY_API_TOKEN: ${{ secrets.FLY_API_TOKEN }}
`, appName)
}
