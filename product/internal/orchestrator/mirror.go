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

// mirrorToGitHub pushes the just-built workspace to the project's private repo
// (source review + a deploy-on-push Action). Best-effort: a failure never
// affects the build. Runs after preview_ready is persisted.
func (o *Orchestrator) mirrorToGitHub(ctx context.Context, p *project.Project, snapshotKey, message string) {
	if o.mirror == nil || snapshotKey == "" {
		return
	}
	raw, err := o.storage.Get(ctx, snapshotKey)
	if err != nil {
		o.log.Error("mirror: read snapshot", "project", p.ID, "err", err)
		return
	}
	files, err := untarGz(raw)
	if err != nil {
		o.log.Error("mirror: untar snapshot", "project", p.ID, "err", err)
		return
	}
	if len(files) == 0 {
		return
	}

	appName := builder.DeployAppName(p.ID)
	files[".github/workflows/deploy.yml"] = []byte(deployWorkflow(appName))

	// The CI deploy token is longer-lived than the build token and lives only
	// as the repo's encrypted FLY_API_TOKEN secret.
	ciToken, err := o.machines.RepoDeployToken(ctx, appName)
	if err != nil {
		o.log.Error("mirror: ci token", "project", p.ID, "err", err)
	}

	url, err := o.mirror.Push(ctx, github.PushSpec{
		Repo: appName, Message: message, Files: files, FlyToken: ciToken,
	})
	if err != nil {
		o.log.Error("mirror: push", "project", p.ID, "err", err)
		return
	}
	p.RepoURL = url
	if err := o.save(ctx, p); err != nil {
		o.log.Error("mirror: save repo url", "project", p.ID, "err", err)
	}
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
