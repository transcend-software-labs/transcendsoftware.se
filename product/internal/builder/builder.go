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
	"path"
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
	// AssetNotes is the customer's own words on what each uploaded file is
	// ("our logo", "photo of the shop front") — appended to the instruction so
	// the agent places files deliberately instead of guessing from filenames.
	AssetNotes string
	// ProgressNote asks the agent to emit "FORGE_PAGE_DONE: <slug>" as it
	// finishes each planned page, so the customer's live checklist ticks off
	// authoritatively (heuristics only tell us what's in progress).
	ProgressNote string

	// OwnerEmail is the Forge customer's email. Injected as the app's
	// OWNER_EMAIL secret so the generated site reserves its first — owner —
	// account for that address (see the template's signup flow).
	OwnerEmail string
	// SiteName is the project name — the site's SITE_NAME (notification copy)
	// and the display name on its notification sender.
	SiteName string

	// Model overrides the implementation model for this build (operator
	// experiment). Zero value → the builder's configured default (AnthropicKey /
	// LLM*), i.e. current behavior.
	Model ModelSpec

	// Agent selects which coding agent executes the build: "" = opencode (the
	// default, driven over its HTTP server), AgentGrok = xAI's Grok Build CLI
	// run headless inside the same sandbox. Model is ignored for grok — it uses
	// the builder's configured GrokModel.
	Agent string
}

// AgentGrok selects the Grok Build CLI as the build agent.
const AgentGrok = "grok"

// agentRunner executes one agent pass over /workspace with an instruction.
// Both the main build and the polish fix round go through it, so the whole
// choreography (spawn, restore, deploy verify, snapshot, review, polish) is
// agent-agnostic.
type agentRunner func(ctx context.Context, instruction string) (opencode.Result, error)

// ModelSpec is a per-build implementation-model override. Provider is
// "anthropic" (native, opencode's anthropic provider) or "zen"/other
// (OpenAI-compatible gateway). Empty Provider → the builder's default.
type ModelSpec struct {
	Provider string
	BaseURL  string
	APIKey   string
	Model    string
	Effort   string
	NativeGo bool // route the impl via opencode's native "opencode-go" provider
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
	Findings      []Finding      // impeccable design-audit findings (non-nil iff the audit ran; empty = clean)
	Tokens        int            // total model tokens consumed by the build agent
	TokensInput   int            // input-only subset of Tokens (for accurate cost)
	Critique      string         // in-session visual-review verdict (SHIP / POLISH + notes), "" if it didn't run
}

// Finding is one issue the impeccable design detector reported on the built UI.
// Fields mirror impeccable's --json output verbatim.
type Finding struct {
	Antipattern string `json:"antipattern"` // rule id, e.g. "ai-color-palette"
	Name        string `json:"name"`        // human title
	Description string `json:"description"` // why it's a problem + how to fix
	Severity    string `json:"severity"`    // "warning", "error", …
	File        string `json:"file"`        // repo-relative file (sandbox prefix stripped)
	Line        int    `json:"line"`        // 0 when file-level
	Snippet     string `json:"snippet"`     // the offending context
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

	// Grok Build (headless) as an alternative agent — see Request.Agent. The
	// CLI is baked into the sandbox image; the key is injected per build, only
	// for grok builds.
	GrokAPIKey string
	GrokModel  string // -m value (default grok-4.5 when empty)
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
	// The implementation model: a per-build override (operator experiment) wins;
	// otherwise the builder's configured default (current behavior). The
	// entrypoint turns these env vars into opencode's provider config.
	switch {
	case req.Model.Provider == "anthropic":
		env["ANTHROPIC_API_KEY"] = req.Model.APIKey
		env["IMPL_PROVIDER"] = "anthropic" // entrypoint writes an explicit anthropic model block
		env["ANTHROPIC_MODEL"] = req.Model.Model
		if req.Model.Effort != "" {
			env["ANTHROPIC_EFFORT"] = req.Model.Effort
		}
	case req.Model.Provider != "": // zen / OpenAI-compatible gateway
		env["LLM_API_KEY"] = req.Model.APIKey
		env["LLM_BASE_URL"] = req.Model.BaseURL
		env["LLM_MODEL"] = req.Model.Model
		if req.Model.Effort != "" {
			env["LLM_EFFORT"] = req.Model.Effort
		}
		if req.Model.NativeGo {
			// Use opencode's native opencode-go provider (full model list +
			// per-model endpoint routing), not the lite openai-compatible shim.
			env["IMPL_GO_NATIVE"] = "1"
		}
	default: // no override → the configured global default
		if b.cfg.AnthropicKey != "" {
			env["ANTHROPIC_API_KEY"] = b.cfg.AnthropicKey
		}
		if b.cfg.LLMKey != "" {
			env["LLM_API_KEY"] = b.cfg.LLMKey
			env["LLM_BASE_URL"] = b.cfg.LLMBaseURL
			env["LLM_MODEL"] = b.cfg.LLMModel
		}
	}
	if req.Agent == AgentGrok {
		// The key rides the machine env so the Exec-launched CLI sees it; the
		// opencode server idles untouched alongside.
		env["XAI_API_KEY"] = b.cfg.GrokAPIKey
		if b.cfg.GrokModel != "" {
			env["GROK_MODEL"] = b.cfg.GrokModel
		}
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
	if req.AssetNotes != "" {
		instruction += "\n\n" + req.AssetNotes
	}
	if req.ProgressNote != "" {
		instruction += "\n\n" + req.ProgressNote
	}
	if b.cfg.Impeccable {
		instruction += "\n\n" + impeccableStep
	}

	run := b.opencodeRunner(sb, hooks)
	if req.Agent == AgentGrok {
		run = b.grokRunner(sb, hooks)
	}
	res, err := run(ctx, instruction)
	return b.finalize(ctx, sb, req, res, err, hooks, run)
}

// opencodeRunner runs an agent pass through the opencode HTTP server.
func (b *Sandbox) opencodeRunner(sb *fly.Sandbox, hooks Hooks) agentRunner {
	return func(ctx context.Context, instruction string) (opencode.Result, error) {
		return b.newDriver(sb.Addr).Run(ctx, opencode.Spec{
			Workdir:      "/workspace",
			SystemPrompt: b.cfg.SystemPrompt,
			Instruction:  instruction,
		}, hooks.OnLog, hooks.OnSession)
	}
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
	}, res, err, hooks, b.opencodeRunner(sb, hooks))
}

// finalize runs the shared tail after the agent run ends — for both a fresh
// Build and a re-Attach. It does not tear the sandbox down; the caller's defer
// does.
func (b *Sandbox) finalize(ctx context.Context, sb *fly.Sandbox, req Request, res opencode.Result, runErr error, hooks Hooks, run agentRunner) (Result, error) {
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

	// Post-deploy review: screenshots + the agent's visual critique + the
	// orchestrator's own deterministic audit of the DEPLOYED site.
	shots, critique, findings := b.review(ctx, sb, preview, req, hooks)

	// Bounded polish pass: the independent audit and critique are what land in
	// /admin, but if they still flag issues we hand them back to a fresh agent
	// session in the same sandbox for ONE fix + redeploy, then re-review — so
	// the residue is what survives enforcement, not what was skipped. Guarded on
	// the quality gate being on and enough build time remaining.
	if b.cfg.Impeccable && fixWorthwhile(findings, critique) {
		if left := timeLeft(ctx); left >= minFixRoundTime {
			emit(hooks.OnLog, fmt.Sprintf("Polishing: %d audit finding(s)%s — one fix pass, then redeploy…",
				len(findings), map[bool]string{true: " + visual critique", false: ""}[critiqueSaysPolish(critique)]))
			if b.runFixRound(ctx, run, findings, critique, left, hooks, &res) {
				shots, critique, findings = b.review(ctx, sb, preview, req, hooks)
				// The workspace changed — re-snapshot so the next iteration
				// continues from the polished site.
				if req.SnapshotPutURL != "" {
					if err := b.saveSnapshot(ctx, sb.MachineID, req.SnapshotPutURL); err == nil {
						snapshotSaved = true
					}
				}
			}
		} else {
			emit(hooks.OnLog, "Skipping the polish pass — not enough build time left; findings noted for review.")
		}
	}

	return Result{PreviewURL: preview, Log: res.Log, Tokens: res.Tokens, TokensInput: res.TokensInput,
		SnapshotSaved: snapshotSaved, Screenshots: shots, Findings: findings, Critique: critique}, nil
}

// review runs the post-deploy review of the DEPLOYED site: page screenshots,
// the agent's in-session visual critique (design-review.js output), and the
// orchestrator's own deterministic design audit. Re-runnable after a fix pass.
func (b *Sandbox) review(ctx context.Context, sb *fly.Sandbox, preview string, req Request, hooks Hooks) ([]CapturedPage, string, []Finding) {
	var shots []CapturedPage
	if len(req.ScreenshotPutURLs) > 0 {
		emit(hooks.OnLog, "Capturing screenshots of each page…")
		if captured, err := b.captureScreenshots(ctx, sb.MachineID, preview, req.ScreenshotPutURLs); err != nil {
			emit(hooks.OnLog, "Warning: could not capture screenshots.")
		} else {
			shots = captured
			emit(hooks.OnLog, fmt.Sprintf("Captured %d page(s).", len(captured)))
		}
	}

	critique := b.readDesignReview(ctx, sb.MachineID)
	if critique != "" {
		emit(hooks.OnLog, "Visual review:\n"+critique)
	}

	findings, err := b.auditDesign(ctx, sb.MachineID, preview)
	if err != nil {
		emit(hooks.OnLog, "Warning: could not run the design audit.")
	} else if len(findings) == 0 {
		emit(hooks.OnLog, "Design audit: clean ✓")
	} else {
		emit(hooks.OnLog, fmt.Sprintf("Design audit: %d finding(s).", len(findings)))
	}

	// The audit proves the pages render; this proves the money path RUNS —
	// submit the primary public form and require it not to crash (see
	// formcheck.go). A failure lands in findings, so the polish pass fixes it
	// and the operator review shows it.
	formFinding, note := auditPrimaryForm(ctx, preview)
	emit(hooks.OnLog, note)
	if formFinding != nil {
		findings = append(findings, *formFinding)
	}
	return shots, critique, findings
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

// auditDesign records the design-quality findings for Rasmus's /admin review.
//
// It prefers the RENDERED audit: scripts/audit.js crawls the DEPLOYED site,
// inlines each page's CSS and runs the impeccable detector on the real composed
// HTML — catching defects that exist only once a page is assembled (a section
// rule overriding a button's text colour so it's invisible, faded low-contrast
// footer text). A source-file scan cannot see those, so showing its result in
// /admin let a site read "clean ✓" while a customer saw a broken button. When
// the rendered audit can't run (deploy not reachable yet, non-template build),
// it falls back to the source scan so the operator still gets something.
//
// impeccable is baked into the sandbox image (no LLM, no key). A non-nil slice
// (empty when clean) means "audited"; nil means "audit didn't run".
func (b *Sandbox) auditDesign(ctx context.Context, machineID, previewURL string) ([]Finding, error) {
	if previewURL != "" {
		// Fresh run against the deployed site — rm first so we never read a
		// stale file the build agent left from its own pre-deploy audit.
		script := `if [ -f /workspace/scripts/audit.js ]; then ` +
			`rm -f /tmp/forge-audit-findings.json; ` +
			`node /workspace/scripts/audit.js "` + previewURL + `" >/dev/null 2>&1 || true; ` +
			`cat /tmp/forge-audit-findings.json 2>/dev/null || true; fi`
		if res, err := b.machines.Exec(ctx, machineID, []string{"/bin/sh", "-c", script}, 150); err == nil {
			if f, ok := parseFindings(res.Stdout); ok {
				return f, nil
			}
		}
	}
	// Fallback: static source scan (older/non-template builds, or deploy not up).
	script := "cd /workspace && impeccable detect --json internal/web/static internal/web/templates"
	res, err := b.machines.Exec(ctx, machineID, []string{"/bin/sh", "-c", script}, 60)
	if err != nil {
		return nil, fmt.Errorf("design audit: %w", err)
	}
	f, ok := parseFindings(res.Stdout)
	if !ok {
		return nil, fmt.Errorf("design audit: no/invalid output (exit %d): %s", res.ExitCode, res.Stderr)
	}
	return f, nil
}

// readDesignReview returns the last in-session visual-review verdict the agent
// captured (design-review.js writes it to a temp file), or "" if it never ran.
func (b *Sandbox) readDesignReview(ctx context.Context, machineID string) string {
	res, err := b.machines.Exec(ctx, machineID,
		[]string{"/bin/sh", "-c", "cat /tmp/forge-design-review.txt 2>/dev/null || true"}, 20)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(res.Stdout)
}

// parseFindings decodes an impeccable JSON findings array; ok=false when the
// output is empty or unparseable. A nil array normalizes to empty (clean).
func parseFindings(stdout string) ([]Finding, bool) {
	out := strings.TrimSpace(stdout)
	if out == "" {
		return nil, false
	}
	var findings []Finding
	if err := json.Unmarshal([]byte(out), &findings); err != nil {
		return nil, false
	}
	if findings == nil {
		findings = []Finding{} // audited and clean — distinct from "not audited"
	}
	for i := range findings {
		f := strings.TrimPrefix(findings[i].File, "/workspace/")
		// Rendered-audit findings point at throwaway temp files
		// (/tmp/forge-audit-XXXX/page3.html) — keep just the page name.
		if strings.Contains(f, "forge-audit-") {
			f = path.Base(f)
		}
		findings[i].File = f
	}
	return findings, true
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

// Fix-round budget. A polish pass runs a second agent session + redeploy, so
// it only starts with real time to spare, gets its own bounded slice of what's
// left, and is capped so it can't eat the whole remaining build window.
const (
	minFixRoundTime = 15 * time.Minute // don't start a fix round below this
	fixRoundBuffer  = 6 * time.Minute  // reserve for the re-review + snapshot after
	maxFixRound     = 30 * time.Minute // cap on the fix session itself
)

// fixWorthwhile decides whether the deployed site warrants one polish pass: the
// independent audit found real defects, or the visual critic still says POLISH.
func fixWorthwhile(findings []Finding, critique string) bool {
	return len(findings) > 0 || critiqueSaysPolish(critique)
}

// critiqueSaysPolish reports whether the design critic's verdict is POLISH
// (it replies SHIP or POLISH; SHIP contains no "polish").
func critiqueSaysPolish(critique string) bool {
	return strings.Contains(strings.ToUpper(critique), "POLISH")
}

// timeLeft is the build ctx's remaining time (a large value when it has no
// deadline, e.g. tests).
func timeLeft(ctx context.Context) time.Duration {
	if dl, ok := ctx.Deadline(); ok {
		return time.Until(dl)
	}
	return 999 * time.Hour
}

// runFixRound runs one targeted fix + redeploy agent session in the same
// sandbox (workspace intact), bounded well inside the remaining build time.
// Returns true when it completed (so the caller re-reviews). The agent's log
// and token count fold into res.
func (b *Sandbox) runFixRound(ctx context.Context, run agentRunner, findings []Finding, critique string, left time.Duration, hooks Hooks, res *opencode.Result) bool {
	budget := left - fixRoundBuffer
	if budget > maxFixRound {
		budget = maxFixRound
	}
	fctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	// The polish pass runs through the same agent as the build. For opencode it
	// is a NEW session, and the runner reports it via OnSession — whatever
	// session id is persisted is what a restarted orchestrator re-attaches to.
	// Without that, a mid-polish restart re-attaches to the finished main
	// session — the log relay goes silent and the watchdog "finalises" under
	// the still-working polish agent.
	fres, err := run(fctx, fixRoundInstruction(findings, critique))
	res.Log += "\n" + fres.Log
	res.Tokens += fres.Tokens
	res.TokensInput += fres.TokensInput
	if err != nil {
		emit(hooks.OnLog, "Polish pass didn't finish cleanly — keeping the deployed site as-is.")
		return false
	}
	return true
}

// fixRoundInstruction turns the leftover audit findings + visual critique into a
// tight "fix only these, then redeploy" prompt — a polish pass, not a rebuild.
func fixRoundInstruction(findings []Finding, critique string) string {
	var b strings.Builder
	b.WriteString("POLISH PASS — the deployed site was independently reviewed and still has issues. ")
	b.WriteString("Fix ONLY the items below on the already-built site in /workspace, then redeploy. ")
	b.WriteString("Make the smallest change that clears each item — do NOT redesign, restructure, or touch anything not listed.\n")

	if len(findings) > 0 {
		b.WriteString("\nDesign-audit findings (the impeccable detector on the RENDERED, deployed pages — real defects, not optional):\n")
		for i, f := range findings {
			fmt.Fprintf(&b, "%d.", i+1)
			if f.Severity != "" {
				fmt.Fprintf(&b, " [%s]", f.Severity)
			}
			if f.Description != "" {
				fmt.Fprintf(&b, " %s", f.Description)
			} else if f.Name != "" {
				fmt.Fprintf(&b, " %s", f.Name)
			}
			if f.File != "" {
				fmt.Fprintf(&b, " (%s", f.File)
				if f.Line > 0 {
					fmt.Fprintf(&b, ":%d", f.Line)
				}
				b.WriteString(")")
			}
			b.WriteString("\n")
		}
	}
	if c := strings.TrimSpace(critique); c != "" && critiqueSaysPolish(c) {
		b.WriteString("\nDesign director's visual critique — apply the concrete fixes it lists:\n")
		b.WriteString(c + "\n")
	}

	b.WriteString("\nBefore redeploying: serve the app locally (./scripts/serve.sh), re-run `node scripts/audit.js` until it prints clean (or a genuinely-stuck item you note), and re-run `node scripts/design-review.js` and apply what it lists (one more pass, then stop). ")
	b.WriteString("Wire the customer's real assets from /workspace/assets/ so the review judges the real page. Then run fly deploy exactly once. Keep it minimal — this is polish, not a rebuild.")
	return b.String()
}

// impeccableStep appends a deterministic design-quality gate to the build when
// enabled. `impeccable` is baked into the sandbox image (no LLM, no key).
const impeccableStep = `DESIGN-QUALITY GATE — this runs BEFORE the fly deploy command, together with
the browser test, as your last pre-deploy checks. Do NOT deploy first and fix
after: that wastes an entire deploy on an unpolished site.
With the app running locally (./scripts/serve.sh) and the browser tests passing,
audit the RENDERED site — not the source — with the provided script:
  node scripts/audit.js
It crawls your running pages, renders each, and runs the impeccable design
detector on the REAL assembled HTML. This is load-bearing: many defects exist
ONLY once the page is composed and never appear in a single template file — e.g.
a section rule (.section-dark a) overriding a .btn text color so the button is
invisible until hover, or opacity making footer text too faint. Scanning the
template source misses all of these; auditing the rendered page catches them.
Fix EVERY issue it reports and re-run until it prints "clean". A warning is a real
defect (an invisible button, faded text, an overused AI-tell font like Space
Grotesk or Inter, all-caps body text) — NOT optional polish — so do NOT deploy
with unresolved findings; keep fixing and re-running. The bar is a clean report.
Only leave a finding if it directly conflicts with the customer's explicit stated
design choice (then note why), or if after a few honest attempts that one specific
finding genuinely will not clear.

Then SEE your own work: with the app still running locally, run
  node scripts/design-review.js
It screenshots your pages and a design director (a vision model) critiques the
REAL rendered look — hierarchy, balance, whether it reads as intentionally
designed or generic — things a linter can't judge. If it replies POLISH, apply
the concrete fixes it lists (edit CSS/templates, not the plan), then run it again
to confirm. Do at most TWO polish passes: land the clear wins, then stop — don't
chase subjective nitpicks. If it can't run (prints that it's skipping), just rely
on audit.js. Treat SHIP as the goal.

The site you serve locally IS what deploys — Fly serves the same baked-in static
files and the app seeds its own database, so what audit.js and design-review.js
see on localhost is exactly what the customer will get. There is no meaningful
"review it after it's live"; get it right here. Concretely: wire the customer's
real assets in BEFORE you review — their uploaded and AI-generated images are
already downloaded in /workspace/assets/ — so the review judges the real page
(real logo, real photos in place), not placeholders. A review of a site still
showing stand-in images is worthless.

Run fly deploy only once audit.js is clean (or down to such a noted exception)
and the local visual review — with the real images in place — is done.`

// resumePreamble tells the agent the workspace holds an interrupted build's
// progress: finish and deploy, don't redo completed work.
const resumePreamble = `The workspace /workspace already contains your PREVIOUS, INTERRUPTED build of
this site — it stopped before it finished deploying. Review what's there,
complete anything unfinished, make sure it builds and the tests pass, then
deploy it. Do NOT start over or redo work that's already done — prioritise
getting the existing site deployed. The plan it was built against:`

// restoreSnapshot unpacks the previous build's workspace into /workspace.
func (b *Sandbox) restoreSnapshot(ctx context.Context, machineID, getURL string) error {
	// Download to a file first, then extract: a mid-stream failure piped straight
	// into tar is unretryable (tar already consumed partial input). --http1.1
	// because HTTP/2 to S3 gateways intermittently dies with PROTOCOL_ERROR
	// (curl exit 92) — it killed real builds; retries cover the rest.
	script := `curl -fsSL --http1.1 --retry 3 --retry-all-errors -o /tmp/seed.tgz '` + getURL + `'` +
		` && tar -xzf /tmp/seed.tgz -C /workspace && rm -f /tmp/seed.tgz`
	res, err := b.machines.Exec(ctx, machineID, []string{"/bin/sh", "-c", script}, 120)
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
		`curl -fsS --http1.1 --retry 3 --retry-all-errors -T /tmp/snapshot.tgz -o /dev/null '` + putURL + `'`
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
