// Package builder runs one build pass: spawn an isolated sandbox, restore the
// previous workspace snapshot (on reiterations), drive opencode inside it to
// build the site, let the agent deploy it, save a new snapshot, and tear the
// sandbox down.
//
// The opencode driver is built per task from the spawned sandbox's address, so
// the same Sandbox builder works in dev mode (fake machines → empty address →
// fake driver) and in real mode (a Fly Machine → private address → HTTP driver).
//
// Credentials in the sandbox: the LLM API key (opencode needs it) and FLY_APP +
// FLY_DEPLOY_TOKEN so the agent can run `fly deploy`. The deploy token is minted
// per build, scoped to that one customer app (see fly.HTTP.AppDeployToken).
// Storage is never credentialed: assets and snapshots move through short-lived
// presigned URLs only.
package builder

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/fly"
	"github.com/transcend-software-labs/rasmus-ai/internal/opencode"
)

// DeployAppName is the per-customer Fly app for a project (globally unique).
// Exported so the orchestrator's reaper and the admin destroy action can name
// the app to remove.
func DeployAppName(projectID string) string {
	id := strings.ToLower(projectID)
	if len(id) > 12 {
		id = id[:12]
	}
	return "forge-" + id
}

// Request is one build pass.
type Request struct {
	ProjectID string
	Brief     string
	Plan      string
	Prompt    string // empty for the initial build; the change request on reiterations

	// SnapshotGetURL, when set, is a presigned GET URL of the workspace snapshot
	// from the previous successful build; it is restored into /workspace before
	// the agent runs, so reiterations edit the existing site instead of
	// rebuilding from scratch.
	SnapshotGetURL string
	// TemplateGetURL, when set (and there is no snapshot), is a presigned GET
	// URL of the starter-app tarball unpacked into /workspace before the first
	// build — the agent extends a working app instead of scaffolding.
	TemplateGetURL string
	// SnapshotPutURL, when set, is a presigned PUT URL the workspace is uploaded
	// to after a successful build. Both URLs keep storage credentials out of the
	// sandbox (same model as asset downloads).
	SnapshotPutURL string
	// ScreenshotPutURLs are presigned PUT URLs (one per slot) for screenshots of
	// the deployed site's pages, captured in-sandbox (Playwright) for Rasmus's
	// review. The crawler fills as many slots as there are pages, up to len().
	ScreenshotPutURLs []string

	AssetManifest map[string]string // filename → short-lived presigned GET URL

	// OwnerEmail is the Forge customer's email. Injected as the app's
	// OWNER_EMAIL secret so the generated site reserves its first — owner —
	// account for that address (see the template's signup flow).
	OwnerEmail string
	// SiteName is the project name — the site's SITE_NAME (notification copy)
	// and the display name on its notification sender.
	SiteName string
}

// CapturedPage is one screenshot the crawler produced: which slot (PUT URL
// index) it was uploaded to and the site path it shows.
type CapturedPage struct {
	Slot int    `json:"slot"`
	Path string `json:"path"`
}

// Result is the outcome of a build pass.
type Result struct {
	PreviewURL    string
	Log           string
	SnapshotSaved bool           // the workspace snapshot was uploaded to SnapshotPutURL
	Screenshots   []CapturedPage // pages captured (slot → path)
	Tokens        int            // model tokens consumed by the build agent
}

// Hooks observe a build pass.
type Hooks struct {
	OnLog     func(string)                 // progress lines, live
	OnSandbox func(machineID, addr string) // called once the sandbox is spawned
	OnSession func(sessionID string)       // called once the opencode session exists (for re-attach)
}

// AttachRequest re-connects to a build already running in an existing sandbox
// (after an orchestrator restart) and finishes it — everything needed to reach
// the live opencode session and to finalise once it completes.
type AttachRequest struct {
	ProjectID         string
	MachineID         string // the still-running sandbox
	Addr              string // its opencode address (http://[ip]:port)
	SessionID         string // the running opencode session
	SnapshotPutURL    string // where to save the finished workspace
	ScreenshotPutURLs []string
}

// Builder runs a build pass.
type Builder interface {
	Build(ctx context.Context, req Request, hooks Hooks) (Result, error)
	// Attach finishes a build already running in an existing sandbox.
	Attach(ctx context.Context, req AttachRequest, hooks Hooks) (Result, error)
}

// Config holds the sandbox builder's settings.
type Config struct {
	SystemPrompt string // "Rasmus's decisions" operating spec, passed to the agent
	OpencodePort int    // port opencode listens on inside the sandbox
	// AnthropicKey is injected so opencode can call Claude (if used).
	AnthropicKey string
	// LLM* configure an OpenAI-compatible model for opencode (e.g. Moonshot/Kimi).
	// The entrypoint writes an opencode provider config from these. The LLM key
	// and the deploy token (FLY_APP/FLY_DEPLOY_TOKEN, injected in Build) are the
	// only credentials inside the sandbox.
	LLMBaseURL string
	LLMKey     string
	LLMModel   string

	// Backup* configure per-app litestream replication of the deployed site's
	// SQLite database to object storage (empty bucket → disabled). Injected as
	// app secrets by the orchestrator — never part of the sandbox build env.
	BackupBucket    string
	BackupEndpoint  string
	BackupRegion    string
	BackupAccessKey string
	BackupSecretKey string

	// SitesEmail* enable the generated site's notification hooks (email). Empty
	// key → the site deploys without an email sender. Injected as app secrets.
	SitesEmailKey  string
	SitesEmailFrom string // sender address; the site name becomes the display name

	// Impeccable adds the design-quality gate to the build instruction (the
	// agent runs the impeccable detector on its UI and fixes findings before
	// deploying). The tool is baked into the sandbox image.
	Impeccable bool
}

// DriverFactory builds an opencode driver for a sandbox at the given address.
// An empty address (dev/fake mode) should yield a fake driver.
type DriverFactory func(addr string) opencode.Driver

// Sandbox builds inside an isolated, per-task sandbox.
type Sandbox struct {
	machines  fly.Machines
	newDriver DriverFactory
	cfg       Config
}

// NewSandbox wires a sandboxed builder.
func NewSandbox(machines fly.Machines, newDriver DriverFactory, cfg Config) *Sandbox {
	if cfg.OpencodePort == 0 {
		cfg.OpencodePort = fly.DefaultPort
	}
	return &Sandbox{machines: machines, newDriver: newDriver, cfg: cfg}
}

// Build spawns a sandbox, runs the agent, deploys, and tears the sandbox down.
func (b *Sandbox) Build(ctx context.Context, req Request, hooks Hooks) (Result, error) {
	env := map[string]string{}
	// Playwright is installed GLOBALLY in the sandbox image (npm i -g), but
	// require('playwright') from a script outside a node_modules tree can't
	// resolve a global package without NODE_PATH. Preset it (both common global
	// roots) so the agent's browser test "just works" instead of burning several
	// tool-calls rediscovering it every build.
	env["NODE_PATH"] = "/usr/lib/node_modules:/usr/local/lib/node_modules"
	if b.cfg.AnthropicKey != "" {
		env["ANTHROPIC_API_KEY"] = b.cfg.AnthropicKey
	}
	if b.cfg.LLMKey != "" {
		env["LLM_API_KEY"] = b.cfg.LLMKey
		env["LLM_BASE_URL"] = b.cfg.LLMBaseURL
		env["LLM_MODEL"] = b.cfg.LLMModel
	}
	if len(req.AssetManifest) > 0 {
		// Presigned GET URLs — the entrypoint downloads these into the workspace.
		// No storage credentials enter the sandbox.
		if b, err := json.Marshal(req.AssetManifest); err == nil {
			env["ASSETS_MANIFEST"] = string(b)
		}
	}

	// Provision the per-customer app (orchestrator side) and inject the app name
	// + a deploy token scoped to just that app (minted per build) so the agent
	// can `fly deploy` it and nothing else — see fly.HTTP.AppDeployToken.
	appName := DeployAppName(req.ProjectID)
	if err := b.machines.EnsureApp(ctx, appName); err != nil {
		return Result{}, err
	}
	// Inject the customer app's runtime secrets — orchestrator-side, never in
	// the sandbox env. Best-effort: a gap here must not fail the build.
	// - LITESTREAM_*: litestream replicates the site's SQLite DB to object
	//   storage (durable across volume/host loss); path = appName so each site
	//   backs up to its own prefix.
	// - OWNER_EMAIL: the generated site reserves its first (owner) account for
	//   the ordering customer.
	appSecrets := map[string]string{}
	if b.cfg.BackupBucket != "" {
		appSecrets["LITESTREAM_BUCKET"] = b.cfg.BackupBucket
		appSecrets["LITESTREAM_ENDPOINT"] = b.cfg.BackupEndpoint
		appSecrets["LITESTREAM_REGION"] = b.cfg.BackupRegion
		appSecrets["LITESTREAM_ACCESS_KEY_ID"] = b.cfg.BackupAccessKey
		appSecrets["LITESTREAM_SECRET_ACCESS_KEY"] = b.cfg.BackupSecretKey
		appSecrets["LITESTREAM_PATH"] = appName
	}
	if req.OwnerEmail != "" {
		appSecrets["OWNER_EMAIL"] = req.OwnerEmail
	}
	if req.SiteName != "" {
		appSecrets["SITE_NAME"] = req.SiteName
	}
	if b.cfg.SitesEmailKey != "" && b.cfg.SitesEmailFrom != "" {
		appSecrets["EMAIL_API_KEY"] = b.cfg.SitesEmailKey
		appSecrets["EMAIL_FROM"] = emailFrom(req.SiteName, b.cfg.SitesEmailFrom)
	}
	if len(appSecrets) > 0 {
		if err := b.machines.SetAppSecrets(ctx, appName, appSecrets); err != nil {
			emit(hooks.OnLog, "Note: could not set the app's backup/owner secrets for this build.")
		}
	}
	token, err := b.machines.AppDeployToken(ctx, appName)
	if err != nil {
		return Result{}, err
	}
	env["FLY_APP"] = appName
	if token != "" {
		env["FLY_DEPLOY_TOKEN"] = token
	}

	emit(hooks.OnLog, "Spawning isolated sandbox…")
	sb, err := b.machines.SpawnSandbox(ctx, fly.SpawnSpec{
		TaskID: req.ProjectID,
		Port:   b.cfg.OpencodePort,
		Env:    env,
	})
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = b.machines.DestroySandbox(context.WithoutCancel(ctx), sb) }()
	if hooks.OnSandbox != nil {
		hooks.OnSandbox(sb.MachineID, sb.Addr)
	}

	// Seed the workspace before the agent runs — orchestrator-driven (Machines
	// exec), never left to the agent. Reiterations restore the previous build's
	// snapshot; first builds unpack the starter template when one is configured.
	switch {
	case req.SnapshotGetURL != "":
		emit(hooks.OnLog, "Restoring your site from the previous build…")
		if err := b.restoreSnapshot(ctx, sb.MachineID, req.SnapshotGetURL); err != nil {
			return Result{}, err
		}
	case req.TemplateGetURL != "":
		emit(hooks.OnLog, "Preparing the Forge starter app…")
		if err := b.restoreSnapshot(ctx, sb.MachineID, req.TemplateGetURL); err != nil {
			return Result{}, err
		}
	}
	emit(hooks.OnLog, "Sandbox ready, starting the agent…")

	instruction := req.Plan
	switch {
	case req.Prompt != "":
		instruction = "Apply this change to the existing site, then redeploy:\n\n" + req.Prompt
	case req.SnapshotGetURL != "":
		// Resuming an interrupted build: the workspace already holds the
		// work-in-progress, so finish it and deploy rather than starting over.
		instruction = resumePreamble + "\n\n" + req.Plan
	case req.TemplateGetURL != "":
		instruction = templatePreamble + "\n\n" + req.Plan
	}
	if b.cfg.Impeccable {
		instruction += "\n\n" + impeccableStep
	}

	driver := b.newDriver(sb.Addr)
	res, err := driver.Run(ctx, opencode.Spec{
		Workdir:      "/workspace",
		SystemPrompt: b.cfg.SystemPrompt,
		Instruction:  instruction,
	}, hooks.OnLog, hooks.OnSession)
	return b.finalize(ctx, sb, req, res, err, hooks)
}

// Attach re-connects to a build already running in an existing sandbox (after an
// orchestrator restart) and finishes it — no spawn, no new session, no re-prompt.
// The sandbox and its opencode session kept running while the orchestrator was
// down; here it re-opens the event stream and completes through the same tail as
// a fresh build (verify happens in the orchestrator).
func (b *Sandbox) Attach(ctx context.Context, req AttachRequest, hooks Hooks) (Result, error) {
	sb := &fly.Sandbox{MachineID: req.MachineID, Addr: req.Addr}
	defer func() { _ = b.machines.DestroySandbox(context.WithoutCancel(ctx), sb) }()
	if hooks.OnSandbox != nil {
		hooks.OnSandbox(sb.MachineID, sb.Addr)
	}
	driver := b.newDriver(sb.Addr)
	res, err := driver.Attach(ctx, req.SessionID, hooks.OnLog)
	return b.finalize(ctx, sb, Request{
		ProjectID:         req.ProjectID,
		SnapshotPutURL:    req.SnapshotPutURL,
		ScreenshotPutURLs: req.ScreenshotPutURLs,
	}, res, err, hooks)
}

// finalize runs the shared tail after the agent run ends — for both a fresh
// Build and a re-Attach. It does not tear the sandbox down; the caller's defer
// does.
func (b *Sandbox) finalize(ctx context.Context, sb *fly.Sandbox, req Request, res opencode.Result, runErr error, hooks Hooks) (Result, error) {
	if runErr != nil {
		// Preserve whatever the agent built so a Retry resumes from here instead
		// of rebuilding from scratch — a timeout (the common cause) usually means
		// the site is nearly done. Detached context: the build ctx may already be
		// past its deadline, which is often why we're here.
		saved := false
		if req.SnapshotPutURL != "" {
			emit(hooks.OnLog, "Build interrupted — saving progress so it can be resumed…")
			sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 8*time.Minute)
			// The agent is still running — we only stopped reading its stream, so
			// opencode keeps churning and would starve the snapshot tar (why the
			// 120s save timed out before). Stop it first to free the sandbox.
			_, _ = b.machines.Exec(sctx, sb.MachineID,
				[]string{"/bin/sh", "-c", "pkill -9 -f opencode 2>/dev/null; true"}, 20)
			if serr := b.saveSnapshotTimeout(sctx, sb.MachineID, req.SnapshotPutURL, 360); serr != nil {
				emit(hooks.OnLog, "Could not save progress for resume: "+serr.Error())
			} else {
				saved = true
			}
			cancel()
		}
		return Result{Log: res.Log, SnapshotSaved: saved}, runErr
	}

	// Save the workspace so the next iteration can continue from it. A failed
	// snapshot degrades the next change, not this build — the site is deployed.
	snapshotSaved := false
	if req.SnapshotPutURL != "" {
		emit(hooks.OnLog, "Saving workspace snapshot…")
		if err := b.saveSnapshot(ctx, sb.MachineID, req.SnapshotPutURL); err != nil {
			emit(hooks.OnLog, "Warning: could not save the workspace snapshot.")
		} else {
			snapshotSaved = true
		}
	}

	// The agent ran `fly deploy` inside the sandbox (per the operating spec); the
	// app URL is deterministic.
	preview := "https://" + DeployAppName(req.ProjectID) + ".fly.dev"

	// Capture a screenshot of every page of the deployed site for Rasmus's
	// review. Best-effort and done in-sandbox (it has Chromium); a miss just
	// means no thumbnails.
	var shots []CapturedPage
	if len(req.ScreenshotPutURLs) > 0 {
		emit(hooks.OnLog, "Capturing screenshots of each page…")
		captured, err := b.captureScreenshots(ctx, sb.MachineID, preview, req.ScreenshotPutURLs)
		if err != nil {
			emit(hooks.OnLog, "Warning: could not capture screenshots.")
		} else {
			shots = captured
			emit(hooks.OnLog, fmt.Sprintf("Captured %d page(s).", len(captured)))
		}
	}

	return Result{PreviewURL: preview, Log: res.Log, Tokens: res.Tokens,
		SnapshotSaved: snapshotSaved, Screenshots: shots}, nil
}

// crawlerJS crawls a deployed site's same-origin pages and screenshots each
// (one browser session), uploading to the presigned PUT URLs passed as argv.
// It prints a JSON manifest [{slot,path}] of what it captured.
const crawlerJS = `const { chromium } = require('playwright');
const https = require('https');
const { URL } = require('url');
const [baseURL, ...putURLs] = process.argv.slice(2);
function put(url, buf) {
  return new Promise((resolve, reject) => {
    const req = https.request(new URL(url), { method: 'PUT',
      headers: { 'content-type': 'image/png', 'content-length': buf.length } }, res => {
      res.resume();
      res.on('end', () => (res.statusCode < 300 ? resolve() : reject(new Error('PUT ' + res.statusCode))));
    });
    req.on('error', reject); req.end(buf);
  });
}
(async () => {
  const browser = await chromium.launch();
  const page = await browser.newPage({ viewport: { width: 1280, height: 900 } });
  const origin = new URL(baseURL).origin;
  await page.goto(baseURL, { waitUntil: 'networkidle', timeout: 30000 });
  const hrefs = await page.$$eval('a[href]', els => els.map(a => a.href));
  const paths = ['/'];
  for (const h of hrefs) {
    try {
      const u = new URL(h);
      if (u.origin !== origin) continue;
      if (/\.[a-z0-9]{2,4}$/i.test(u.pathname) && !/\.html?$/i.test(u.pathname)) continue;
      const p = u.pathname.replace(/\/+$/, '') || '/';
      if (!paths.includes(p)) paths.push(p);
    } catch {}
  }
  const list = paths.slice(0, putURLs.length);
  const captured = [];
  for (let i = 0; i < list.length; i++) {
    try {
      await page.goto(origin + list[i], { waitUntil: 'networkidle', timeout: 30000 });
      const buf = await page.screenshot({ fullPage: true });
      await put(putURLs[i], buf);
      captured.push({ slot: i, path: list[i] });
    } catch {}
  }
  await browser.close();
  console.log(JSON.stringify(captured));
})().catch(e => { console.error(e.message || String(e)); process.exit(1); });`

// captureScreenshots writes the crawler into the sandbox and runs it, returning
// the pages it captured. playwright + browsers are baked into the image; the
// global module path is on NODE_PATH so require('playwright') resolves.
func (b *Sandbox) captureScreenshots(ctx context.Context, machineID, siteURL string, putURLs []string) ([]CapturedPage, error) {
	args := shellQuote(siteURL)
	for _, u := range putURLs {
		args += " " + shellQuote(u)
	}
	script := "echo " + shellQuote(base64.StdEncoding.EncodeToString([]byte(crawlerJS))) +
		" | base64 -d > /tmp/crawl.js && NODE_PATH=/usr/lib/node_modules node /tmp/crawl.js " + args
	res, err := b.machines.Exec(ctx, machineID, []string{"/bin/sh", "-c", script}, 150)
	if err != nil {
		return nil, fmt.Errorf("screenshots: %w", err)
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("screenshots: exit %d: %s", res.ExitCode, res.Stderr)
	}
	var pages []CapturedPage
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &pages); err != nil {
		return nil, fmt.Errorf("screenshots: bad manifest %q: %w", res.Stdout, err)
	}
	return pages, nil
}

// templatePreamble tells the agent the workspace is a working starter app, not
// an empty directory. Prepended to the plan on first builds from the template.
const templatePreamble = `The workspace /workspace already contains our production-ready Go starter app
(one binary serving frontend + backend, SQLite, working auth, contact form).
Read AGENTS.md first, then EXTEND this app to implement the plan below.
Do not scaffold a new project. Keep /healthz, auth and CSRF intact.`

// emailFrom builds a From header, using a sanitized site name as the display
// name (quotes/newlines stripped so they can't break the header).
func emailFrom(siteName, address string) string {
	name := strings.Map(func(r rune) rune {
		if r == '"' || r == '\r' || r == '\n' || r == '<' || r == '>' {
			return -1
		}
		return r
	}, strings.TrimSpace(siteName))
	if name == "" {
		return address
	}
	if len(name) > 80 {
		name = name[:80]
	}
	return `"` + name + `" <` + address + `>`
}

// impeccableStep appends a deterministic design-quality gate to the build when
// enabled. `impeccable` is baked into the sandbox image (no LLM, no key).
const impeccableStep = `DESIGN-QUALITY GATE — required before you deploy:
This sandbox has the "impeccable" design detector. Once the UI is built, run:
  impeccable detect --json internal/web/static internal/web/templates
Fix every issue it reports (they flag AI-generated-design tells and quality
problems — see DESIGN.md and AGENTS.md), then run it again. Repeat at most
TWICE, then deploy. An empty report ([]) means it's clean. Do not let this gate
block the deploy indefinitely — after two fix passes, deploy your best result.`

// resumePreamble tells the agent the workspace holds an interrupted build's
// progress: finish and deploy, don't redo completed work.
const resumePreamble = `The workspace /workspace already contains your PREVIOUS, INTERRUPTED build of
this site — it stopped before it finished deploying. Review what's there,
complete anything unfinished, make sure it builds and the tests pass, then
deploy it. Do NOT start over or redo work that's already done — prioritise
getting the existing site deployed. The plan it was built against:`

// restoreSnapshot unpacks the previous build's workspace into /workspace.
func (b *Sandbox) restoreSnapshot(ctx context.Context, machineID, getURL string) error {
	script := `curl -fsSL '` + getURL + `' | tar -xzf - -C /workspace`
	res, err := b.machines.Exec(ctx, machineID, []string{"/bin/sh", "-c", script}, 60)
	if err != nil {
		return fmt.Errorf("restore snapshot: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("restore snapshot: exit %d: %s", res.ExitCode, res.Stderr)
	}
	return nil
}

// saveSnapshot uploads the workspace (minus reinstallable dependencies) to the
// presigned PUT URL.
func (b *Sandbox) saveSnapshot(ctx context.Context, machineID, putURL string) error {
	return b.saveSnapshotTimeout(ctx, machineID, putURL, 120)
}

// saveSnapshotTimeout tars /workspace and uploads it to the presigned PUT URL,
// with a caller-chosen command timeout (longer on the interrupted-build path,
// where the machine may be recovering from a saturated agent).
func (b *Sandbox) saveSnapshotTimeout(ctx context.Context, machineID, putURL string, timeoutSec int) error {
	script := `cd /workspace && tar --exclude=node_modules --exclude=.cache -czf /tmp/snapshot.tgz . && ` +
		`curl -fsS -T /tmp/snapshot.tgz -o /dev/null '` + putURL + `'`
	res, err := b.machines.Exec(ctx, machineID, []string{"/bin/sh", "-c", script}, timeoutSec)
	if err != nil {
		return fmt.Errorf("save snapshot: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("save snapshot: exit %d: %s", res.ExitCode, res.Stderr)
	}
	return nil
}

func emit(onLog func(string), line string) {
	if onLog != nil {
		onLog(line)
	}
}

// shellQuote wraps s for safe interpolation into a /bin/sh -c command.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
