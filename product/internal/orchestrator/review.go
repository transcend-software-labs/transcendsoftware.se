package orchestrator

// The one-shot post-payment code review. When a payment settles (MarkPaid, the
// single choke-point for manual marks and the Stripe webhook alike), the
// project's source snapshot is read from object storage, bundled, and judged
// by the review model as a delivery gate. The verdict lands on the project
// (CodeReview/CodeReviewAt) and surfaces in the operator's review checklist —
// it never blocks or fails the pipeline: a failed review is logged, the
// operator sees "not run" and can trigger it again from /admin.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"
)

// codeReviewTimeout bounds one review call: a snapshot fetch plus a single
// long-context LLM completion.
const codeReviewTimeout = 15 * time.Minute

// StartCodeReview runs the code review in the background. force re-runs it
// even when one already exists (the operator's "run again" button); the
// post-payment trigger passes false so the review stays one-shot.
func (o *Orchestrator) StartCodeReview(projectID string, force bool) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), codeReviewTimeout)
		defer cancel()
		if err := o.runCodeReview(ctx, projectID, force); err != nil {
			o.log.Error("code review", "project", projectID, "err", err)
		}
	}()
}

func (o *Orchestrator) runCodeReview(ctx context.Context, projectID string, force bool) error {
	p, err := o.store.ProjectByID(ctx, projectID)
	if err != nil {
		return err
	}
	if !force && !p.CodeReviewAt.IsZero() {
		return nil // the one-shot already ran
	}
	if p.SnapshotKey == "" {
		// Paid before anything was built. Not an error: finishBuild re-offers
		// the review once the first snapshot exists.
		return nil
	}
	rev, label := o.reviewerFor(p)
	if rev == nil {
		o.log.Warn("code review: no review model available; skipping", "project", p.ID)
		return nil
	}
	raw, err := o.storage.Get(ctx, p.SnapshotKey)
	if err != nil {
		return fmt.Errorf("fetch snapshot: %w", err)
	}
	bundle, err := sourceBundle(raw)
	if err != nil {
		return fmt.Errorf("bundle snapshot: %w", err)
	}
	brief := p.EffectiveBrief()
	if p.Plan != "" {
		brief += "\n\nThe build plan:\n" + p.Plan
	}
	out, err := rev.ReviewCode(ctx, brief, bundle)
	if err != nil {
		return err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return fmt.Errorf("review model returned nothing")
	}
	// Reload before saving — the pipeline may have advanced the project while
	// the model was reading.
	p, err = o.store.ProjectByID(ctx, projectID)
	if err != nil {
		return err
	}
	p.CodeReview = out
	p.CodeReviewAt = time.Now().UTC()
	if err := o.save(ctx, p); err != nil {
		return err
	}
	o.log.Info("code review done", "project", p.ID, "model", label,
		"verdict", strings.Fields(out + " ")[0])
	return nil
}

// Bundle limits. Generated sites are small (the Go starter, extended), so
// these are generous — they exist to keep a runaway asset or generated file
// from blowing the review model's context, not to trim normal source.
const (
	maxReviewFileBytes  = 48 * 1024
	maxReviewTotalBytes = 400 * 1024
)

// reviewSkipDirs are path segments whose subtrees never carry reviewable
// source (dependencies, VCS, caches, build output).
var reviewSkipDirs = map[string]bool{
	"node_modules": true, ".git": true, ".cache": true, "vendor": true,
	"dist": true, "build": true, ".next": true,
}

// reviewExts is the source allowlist; anything else in the snapshot is an
// asset (images, fonts, databases) the review can't read anyway.
var reviewExts = map[string]bool{
	".go": true, ".html": true, ".tmpl": true, ".css": true, ".js": true,
	".ts": true, ".sql": true, ".md": true, ".json": true, ".yaml": true,
	".yml": true, ".toml": true, ".txt": true, ".sh": true, ".env": true,
}

// reviewNames are extensionless files worth reviewing.
var reviewNames = map[string]bool{"Dockerfile": true, "Makefile": true, "Procfile": true}

// sourceBundle turns a workspace snapshot tarball into one reviewable text
// blob: each source file under a "=== FILE: path ===" header, oversized files
// truncated with a marker, the whole bundle capped so a single completion can
// hold it. Skipped/truncated files are listed at the end so the reviewer knows
// what it did not see.
func sourceBundle(tgz []byte) (string, error) {
	gz, err := gzip.NewReader(bytes.NewReader(tgz))
	if err != nil {
		return "", err
	}
	defer gz.Close()

	type entry struct {
		path string
		body []byte
	}
	var files []entry
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		name := path.Clean(strings.TrimPrefix(hdr.Name, "./"))
		if name == "." || name == "" || strings.HasPrefix(name, "..") {
			continue
		}
		skip := false
		for _, seg := range strings.Split(path.Dir(name), "/") {
			if reviewSkipDirs[seg] {
				skip = true
				break
			}
		}
		base := path.Base(name)
		if skip || base == "go.sum" || (!reviewExts[path.Ext(base)] && !reviewNames[base] && !strings.HasPrefix(base, ".env")) {
			continue
		}
		body, err := io.ReadAll(io.LimitReader(tr, maxReviewFileBytes+1))
		if err != nil {
			return "", err
		}
		files = append(files, entry{path: name, body: body})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })

	var b strings.Builder
	var omitted []string
	total := 0
	for _, f := range files {
		if total >= maxReviewTotalBytes {
			omitted = append(omitted, f.path)
			continue
		}
		body, note := f.body, ""
		if len(body) > maxReviewFileBytes {
			body, note = body[:maxReviewFileBytes], " (TRUNCATED)"
		}
		if rem := maxReviewTotalBytes - total; len(body) > rem {
			body, note = body[:rem], " (TRUNCATED)"
		}
		fmt.Fprintf(&b, "=== FILE: %s%s ===\n", f.path, note)
		b.Write(bytes.ToValidUTF8(body, []byte("�")))
		b.WriteString("\n\n")
		total += len(body)
	}
	if len(omitted) > 0 {
		fmt.Fprintf(&b, "=== NOT INCLUDED (bundle size cap): %s ===\n", strings.Join(omitted, ", "))
	}
	if b.Len() == 0 {
		return "", fmt.Errorf("snapshot contains no reviewable source files")
	}
	return b.String(), nil
}

// CodeReviewVerdictClean reports whether a stored review's first word is SHIP.
// Kept here (not on project.Project) so the verdict convention lives next to
// the code that produces it.
func CodeReviewVerdictClean(review string) bool {
	f := strings.Fields(strings.TrimSpace(review))
	return len(f) > 0 && strings.EqualFold(strings.Trim(f[0], ".:!,"), "SHIP")
}
