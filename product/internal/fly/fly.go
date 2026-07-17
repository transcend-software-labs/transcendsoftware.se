// Package fly wraps the Fly Machines + Apps APIs: spawn an ephemeral microVM
// sandbox per build, run orchestrator-driven commands inside it (exec), create
// the per-customer app, and hand out the deploy token the agent uses to
// publish it.
//
// Each Fly Machine is a Firecracker microVM, so one Machine per task is the
// isolation boundary. The org *API* token stays on the trusted side and never
// enters the sandbox. The *deploy* token the sandbox receives is minted per
// build, scoped to that one throwaway customer app (see HTTP.AppDeployToken),
// so a compromised agent can only deploy its own app — with a configured
// org-scoped token as a fallback if minting isn't available.
package fly

import (
	"context"
	"strings"
	"sync"
	"time"
)

// SpawnSpec describes one sandbox to create.
type SpawnSpec struct {
	TaskID string
	Port   int               // opencode port (also injected as OPENCODE_PORT)
	Env    map[string]string // env injected into the machine (e.g. ANTHROPIC_API_KEY, REPO_URL)
}

// Sandbox is a running per-task microVM.
type Sandbox struct {
	MachineID string
	AppName   string
	Addr      string // opencode base URL on the private network, e.g. http://[fdaa::3]:4096
}

// ExecResult is the outcome of a command run inside a sandbox machine.
type ExecResult struct {
	ExitCode int32
	Stdout   string
	Stderr   string
}

// Machines manages ephemeral sandboxes and per-customer app provisioning.
type Machines interface {
	// SpawnSandbox creates an isolated microVM for one build task and returns it
	// once opencode is reachable at Sandbox.Addr.
	SpawnSandbox(ctx context.Context, spec SpawnSpec) (*Sandbox, error)
	// DestroySandbox tears the microVM down.
	DestroySandbox(ctx context.Context, s *Sandbox) error
	// Exec runs a command inside a sandbox machine (Fly Machines exec API) and
	// returns its output. Used for deterministic, orchestrator-driven steps —
	// restoring and saving workspace snapshots — that must not rely on the agent.
	Exec(ctx context.Context, machineID string, command []string, timeoutSec int) (ExecResult, error)
	// EnsureApp creates the per-customer Fly app if it doesn't exist. Done by the
	// orchestrator so app-creation privilege stays out of the sandbox.
	EnsureApp(ctx context.Context, appName string) error
	// SetAppSecrets sets runtime secrets on a per-customer app. Orchestrator
	// side (never the sandbox); used to inject the per-app backup credentials
	// the deployed site's litestream uses. Applied on the app's next deploy.
	SetAppSecrets(ctx context.Context, appName string, secrets map[string]string) error
	// DestroyApp deletes a per-customer app and everything in it (machines,
	// IPs). Destroying an already-absent app is not an error — the reaper and
	// the admin destroy action must be idempotent.
	DestroyApp(ctx context.Context, appName string) error
	// SweepSandboxes destroys leaked sandbox machines, returning how many were
	// reaped. A machine older than grace whose ID is not in keep has no active
	// build driving it — an orphan (e.g. a restart interrupted its teardown) —
	// and every orphan burns an agent's tokens until killed. Machines in keep
	// belong to running builds and survive until max, the hard backstop no
	// legitimate build outlives (the pipeline timeout is well under it).
	SweepSandboxes(ctx context.Context, grace, max time.Duration, keep []string) (int, error)
	// AppDeployToken returns a deploy token for appName, injected into the
	// sandbox so the agent can run `fly deploy`. Scoped to appName alone, minted
	// per build (see HTTP.AppDeployToken).
	AppDeployToken(ctx context.Context, appName string) (string, error)
	// RepoDeployToken returns a longer-lived app-scoped deploy token for the
	// project's GitHub Action (deploy-on-push). "" if not available.
	RepoDeployToken(ctx context.Context, appName string) (string, error)

	// AddCertificate requests an ACME (Let's Encrypt) certificate for hostname on
	// appName and returns the DNS records the customer must set for it to
	// validate (from the Machines API's dns_requirements).
	AddCertificate(ctx context.Context, appName, hostname string) (CertRequirements, error)
	// CheckCertificate forces a validation re-check and reports whether the cert
	// is issued and its DNS is configured, plus the (possibly refreshed) records.
	CheckCertificate(ctx context.Context, appName, hostname string) (CertStatus, error)
	// DeleteCertificate removes the hostname's certificate. An absent cert (404)
	// is not an error — detaching must be idempotent.
	DeleteCertificate(ctx context.Context, appName, hostname string) error
	// AllocateIPv6 allocates a dedicated IPv6 on appName (needed to serve an apex
	// domain via A/AAAA) and returns the address. Callers guard re-allocation on
	// a stored address, so this need not be internally idempotent.
	AllocateIPv6(ctx context.Context, appName string) (string, error)
}

// CertRecord is one DNS record a custom domain needs — ready both to show the
// customer (BYOD) and to push to Cloudflare (purchased).
type CertRecord struct {
	Type  string // A | AAAA | CNAME | TXT
	Name  string // record host (FQDN), e.g. "acme.se", "_acme-challenge.acme.se"
	Value string // target / content
}

// CertRequirements is what a hostname needs to validate: the DNS records to set
// and whether it's an apex (A/AAAA, needs a dedicated IP) or a subdomain (CNAME).
type CertRequirements struct {
	Hostname string
	IsApex   bool
	Records  []CertRecord
}

// CertStatus is a certificate's validation state.
type CertStatus struct {
	Configured    bool // issued and serving
	DNSConfigured bool // the validation records are visible in DNS
	Status        string
	Requirements  CertRequirements // refreshed records, for re-display
}

// DefaultPort is the opencode port used when a spec leaves Port unset.
const DefaultPort = 4096

// Fake is a dev-mode Machines that touches no real infra. It records Exec,
// DestroyApp and SetAppSecrets calls so tests can assert snapshot, reaper and
// backup-provisioning behavior.
type Fake struct {
	mu            sync.Mutex
	execs         []FakeExec
	destroyedApps []string
	appSecrets    map[string]map[string]string
	certs         map[string]bool      // "app|hostname" → cert requested
	certActive    bool                 // what CheckCertificate reports (settable to drive poller tests)
	allocatedIPv6 []string             // apps a dedicated IPv6 was allocated on
	sandboxes     map[string]time.Time // live sandbox machines: id → created (drives sweep tests)
}

// FakeExec is one recorded Exec call.
type FakeExec struct {
	MachineID string
	Command   []string
}

// NewFake returns a dev-mode Machines.
func NewFake() *Fake { return &Fake{} }

func (f *Fake) SpawnSandbox(_ context.Context, spec SpawnSpec) (*Sandbox, error) {
	id := "dev-machine-" + spec.TaskID
	f.mu.Lock()
	if f.sandboxes == nil {
		f.sandboxes = map[string]time.Time{}
	}
	f.sandboxes[id] = time.Now()
	f.mu.Unlock()
	// Addr empty → the driver factory uses the fake opencode driver (dev mode).
	return &Sandbox{MachineID: id, AppName: "dev-app", Addr: ""}, nil
}

func (f *Fake) DestroySandbox(_ context.Context, sb *Sandbox) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sandboxes, sb.MachineID)
	return nil
}

// AddSandboxMachine registers a machine with a chosen creation time — tests
// use it to simulate leaked/orphaned sandboxes.
func (f *Fake) AddSandboxMachine(id string, created time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sandboxes == nil {
		f.sandboxes = map[string]time.Time{}
	}
	f.sandboxes[id] = created
}

// SandboxMachines lists the ids of machines still alive.
func (f *Fake) SandboxMachines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]string, 0, len(f.sandboxes))
	for id := range f.sandboxes {
		ids = append(ids, id)
	}
	return ids
}
func (f *Fake) EnsureApp(_ context.Context, _ string) error { return nil }

func (f *Fake) SetAppSecrets(_ context.Context, appName string, secrets map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.appSecrets == nil {
		f.appSecrets = map[string]map[string]string{}
	}
	f.appSecrets[appName] = secrets
	return nil
}

// AppSecrets returns the secrets recorded for an app (test helper).
func (f *Fake) AppSecrets(appName string) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.appSecrets[appName]
}

func (f *Fake) AppDeployToken(_ context.Context, _ string) (string, error)  { return "", nil }
func (f *Fake) RepoDeployToken(_ context.Context, _ string) (string, error) { return "", nil }

func (f *Fake) DestroyApp(_ context.Context, appName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.destroyedApps = append(f.destroyedApps, appName)
	return nil
}

// DestroyedApps returns the app names destroyed so far.
func (f *Fake) DestroyedApps() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.destroyedApps))
	copy(out, f.destroyedApps)
	return out
}

// SweepSandboxes mirrors the real client's logic over the tracked machines.
func (f *Fake) SweepSandboxes(_ context.Context, grace, max time.Duration, keep []string) (int, error) {
	kept := make(map[string]bool, len(keep))
	for _, id := range keep {
		kept[id] = true
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	reaped := 0
	for id, created := range f.sandboxes {
		age := time.Since(created)
		if age <= grace || (kept[id] && age <= max) {
			continue
		}
		delete(f.sandboxes, id)
		reaped++
	}
	return reaped, nil
}

func (f *Fake) Exec(_ context.Context, machineID string, command []string, _ int) (ExecResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execs = append(f.execs, FakeExec{MachineID: machineID, Command: command})
	// The screenshot crawler expects a JSON manifest on stdout; return a
	// deterministic two-page one so dev/tests exercise the capture path.
	joined := strings.Join(command, " ")
	if strings.Contains(joined, "crawl.js") {
		return ExecResult{ExitCode: 0, Stdout: `[{"slot":0,"path":"/"},{"slot":1,"path":"/kontakt"}]`}, nil
	}
	// The design audit (rendered audit.js, or the source-scan fallback) expects
	// an impeccable JSON findings array on stdout; return a clean one.
	if strings.Contains(joined, "audit.js") || strings.Contains(joined, "impeccable detect") {
		return ExecResult{ExitCode: 0, Stdout: `[]`}, nil
	}
	return ExecResult{ExitCode: 0}, nil
}

// Execs returns the Exec calls recorded so far.
func (f *Fake) Execs() []FakeExec {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]FakeExec, len(f.execs))
	copy(out, f.execs)
	return out
}

// AddCertificate records the request and returns canned apex requirements (an
// A + AAAA + the ACME-challenge CNAME) so orchestrator tests exercise the
// records-and-IP path.
func (f *Fake) AddCertificate(_ context.Context, appName, hostname string) (CertRequirements, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.certs == nil {
		f.certs = map[string]bool{}
	}
	f.certs[appName+"|"+hostname] = true
	return CertRequirements{
		Hostname: hostname,
		IsApex:   true,
		Records: []CertRecord{
			{Type: "A", Name: hostname, Value: "66.0.0.1"},
			{Type: "AAAA", Name: hostname, Value: "2a09:8280:1::99"},
			{Type: "CNAME", Name: "_acme-challenge." + hostname, Value: hostname + ".flydns.net"},
		},
	}, nil
}

// CheckCertificate reports Configured/DNSConfigured according to the settable
// certActive flag (see SetCertActive).
func (f *Fake) CheckCertificate(_ context.Context, _, hostname string) (CertStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	status := "pending_validation"
	if f.certActive {
		status = "active"
	}
	return CertStatus{
		Configured:    f.certActive,
		DNSConfigured: f.certActive,
		Status:        status,
		Requirements:  CertRequirements{Hostname: hostname, IsApex: true},
	}, nil
}

// SetCertActive flips what CheckCertificate reports, to drive poller tests from
// pending → active.
func (f *Fake) SetCertActive(active bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.certActive = active
}

// HasCert reports whether a certificate was requested (and not deleted).
func (f *Fake) HasCert(appName, hostname string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.certs[appName+"|"+hostname]
}

func (f *Fake) DeleteCertificate(_ context.Context, appName, hostname string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.certs, appName+"|"+hostname)
	return nil
}

func (f *Fake) AllocateIPv6(_ context.Context, appName string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.allocatedIPv6 = append(f.allocatedIPv6, appName)
	return "2a09:8280:1::1", nil
}

// AllocatedIPv6 returns the apps a dedicated IPv6 was allocated on.
func (f *Fake) AllocatedIPv6() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.allocatedIPv6))
	copy(out, f.allocatedIPv6)
	return out
}
