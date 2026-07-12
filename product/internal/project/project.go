// Package project holds the core domain types and the project state machine.
//
// A project moves through a fixed lifecycle driven by the orchestrator:
//
//	created → planning → screening → (rejected | building) → preview_ready
//	                                                       ↘ failed
//
// From preview_ready the customer may request up to MaxIterations-1 further
// builds (reiterations); each reiteration loops building → preview_ready.
package project

import (
	"errors"
	"strings"
	"time"
)

// Status is the lifecycle state of a project.
type Status string

const (
	StatusCreated          Status = "created"           // just created, nothing run yet
	StatusClarifying       Status = "clarifying"        // intake agent is generating clarifying questions
	StatusNeedsInput       Status = "needs_input"       // waiting for the customer to answer questions
	StatusPlanning         Status = "planning"          // LLM is turning the brief into a plan
	StatusScreening        Status = "screening"         // safety gate is evaluating the request
	StatusAwaitingApproval Status = "awaiting_approval" // plan ready; waiting for the customer to approve the scope
	StatusEscalated        Status = "escalated"         // safety gate escalated; waiting on operator review
	StatusRejected         Status = "rejected"          // safety gate rejected the request (terminal)
	StatusBuilding         Status = "building"          // a sandboxed build is running
	StatusPreviewReady     Status = "preview_ready"     // a build finished, preview link attached
	StatusAccepted         Status = "accepted"          // customer accepted the preview; awaiting Rasmus's final review
	StatusDelivered        Status = "delivered"         // Rasmus reviewed + guaranteed it (terminal, the handover)
	StatusFailed           Status = "failed"            // a build or pipeline step errored (terminal)
	StatusExpired          Status = "expired"           // preview reaped after the retention window (terminal)
)

// MaxIterations is the total number of build passes a project gets:
// one initial build plus two customer reiterations.
const MaxIterations = 3

// Verdict is the outcome of the safety gate.
type Verdict string

const (
	VerdictAllow    Verdict = "allow"    // proceed to build
	VerdictReject   Verdict = "reject"   // refuse outright
	VerdictEscalate Verdict = "escalate" // ambiguous — route to a human (Rasmus)
)

// DomainStatus is the lifecycle of a project's custom domain (see
// internal/orchestrator/domains.go). The zero value means no domain.
type DomainStatus string

const (
	DomainNone        DomainStatus = ""            // no domain attached
	DomainRegistering DomainStatus = "registering" // Cloudflare is registering a purchased domain (async)
	DomainPendingDNS  DomainStatus = "pending_dns" // BYOD: awaiting the customer's DNS records
	DomainVerifying   DomainStatus = "verifying"   // DNS seen; Fly is issuing the certificate
	DomainActive      DomainStatus = "active"      // certificate issued, domain serving
	DomainFailed      DomainStatus = "failed"      // registration/verification gave up (operator alerted)
)

// Domain kinds: a customer's own domain vs one bought in-app via Cloudflare.
const (
	DomainKindBYOD      = "byod"
	DomainKindPurchased = "purchased"
)

// DomainRecord is one DNS record shown in the domain panel — the customer sets
// it (BYOD) or we set it automatically (purchased). Note is a short per-record
// hint. Every record we create in Cloudflare is proxied:false.
type DomainRecord struct {
	Type  string `json:"type"`  // A | AAAA | CNAME | TXT
	Name  string `json:"name"`  // host, e.g. "@", "www", "_acme-challenge"
	Value string `json:"value"` // target/content
	Note  string `json:"note,omitempty"`
}

// DesignOption is one suggested visual direction from the intake step. The
// customer picks one — or states their own preference — before building.
type DesignOption struct {
	Name        string `json:"name"`        // short label, e.g. "Varmt & rustikt"
	Description string `json:"description"` // one sentence: mood, colors, typography
}

// Screenshot is a captured page of a deployed site: its URL path and the
// object-storage key of the PNG.
type Screenshot struct {
	Path string `json:"path"` // e.g. "/", "/kontakt"
	Key  string `json:"key"`  // object-storage key of the PNG
}

// PlanSpec is the machine-readable companion to the markdown plan: the planner
// emits it so the customer UI (scope card, page checklist, content slots) and
// the build-progress classifier can be driven by structured data instead of
// parsing prose. Empty (no pages) means the planner didn't produce one — the UI
// degrades to not showing these sections rather than breaking.
type PlanSpec struct {
	Pages         []PlanPage    `json:"pages"`
	NotIncluded   []string      `json:"not_included"`   // plain-language things NOT built, for "Ingår inte"
	ContentNeeded []ContentItem `json:"content_needed"` // what we need from the customer
}

// PlanPage is one page/route in the plan. Names is per-locale so a page name
// can slot into a translated sentence ("Building the home page"); Paths are
// lowercase substrings expected in the files/routes the builder creates, used
// to attribute build activity to a page.
type PlanPage struct {
	Slug     string            `json:"slug"`
	Paths    []string          `json:"paths"`
	Names    map[string]string `json:"names"`
	Included string            `json:"included"` // one plain-language phrase, customer's language
}

// ContentItem is one thing the customer must provide. Kind decides how:
//   - "text"   typed in (copy, an email address, opening hours)
//   - "file"   a single uploaded file (a logo)
//   - "files"  several uploaded files (a photo gallery)
//   - "roster" a structured list of people (a team: name, role, bio, photo each)
//
// Image kinds can also carry Generatable, letting the customer create the image
// with AI instead of uploading. Names is per-locale.
type ContentItem struct {
	Slug        string            `json:"slug"`
	Names       map[string]string `json:"names"`
	Required    bool              `json:"required"`
	Kind        string            `json:"kind"`        // text|file|files|roster; empty → inferred
	Generatable bool              `json:"generatable"` // an image slot the AI can generate
}

// imageWords hint that an untagged content item is a file to upload rather than
// text to type (used only when the planner didn't set Kind).
var imageWords = []string{"logo", "logotyp", "photo", "foto", "image", "bild",
	"hero", "picture", "gallery", "galleri", "banner", "icon", "ikon", "illustration",
	"background", "bakgrund", "pattern", "avatar"}

// Type is the resolved kind, honoring Kind when set and otherwise inferring
// from the slug/name (image-ish → file, everything else → text).
func (c ContentItem) Type() string {
	switch c.Kind {
	case "text", "file", "files", "roster":
		return c.Kind
	}
	hay := strings.ToLower(c.Slug + " " + c.Names["en"] + " " + c.Names["sv"])
	for _, w := range imageWords {
		if strings.Contains(hay, w) {
			return "file"
		}
	}
	return "text"
}

// Kind predicates for templates and handlers.
func (c ContentItem) IsText() bool      { return c.Type() == "text" }
func (c ContentItem) IsFile() bool      { return c.Type() == "file" }  // single
func (c ContentItem) IsFiles() bool     { return c.Type() == "files" } // gallery
func (c ContentItem) IsRoster() bool    { return c.Type() == "roster" }
func (c ContentItem) AcceptsFile() bool { t := c.Type(); return t == "file" || t == "files" }

// hasImageWord reports whether the slug/name reads as an image.
func (c ContentItem) hasImageWord() bool {
	hay := strings.ToLower(c.Slug + " " + c.Names["en"] + " " + c.Names["sv"])
	for _, w := range imageWords {
		if strings.Contains(hay, w) {
			return true
		}
	}
	return false
}

// CanGenerate reports whether this image slot offers AI generation. Honors the
// planner's Generatable flag; otherwise offers it for image file slots.
func (c ContentItem) CanGenerate() bool {
	if !c.AcceptsFile() {
		return false // never for text or roster
	}
	return c.Generatable || c.hasImageWord()
}

// RosterEntry is one person in a "roster" content item (a team member): the
// structured fields plus the object-storage key of their photo, if provided.
type RosterEntry struct {
	Name     string `json:"name"`
	Role     string `json:"role"`
	Bio      string `json:"bio"`
	PhotoKey string `json:"photo_key,omitempty"`
}

// ImageCandidates is a pending set of AI-generated images for a slot, awaiting
// the customer's pick. Prompt is what produced them; Keys are their
// object-storage keys.
type ImageCandidates struct {
	Prompt string   `json:"prompt"`
	Keys   []string `json:"keys"`
}

// Name returns the page's display name in lang, falling back to English then
// the slug — so it is always safe to interpolate into UI copy.
func (p PlanPage) Name(lang string) string { return localName(p.Names, lang, p.Slug) }

// Name returns the content item's label in lang, with the same fallbacks.
func (c ContentItem) Name(lang string) string { return localName(c.Names, lang, c.Slug) }

func localName(names map[string]string, lang, fallback string) string {
	if v := names[lang]; v != "" {
		return v
	}
	if v := names["en"]; v != "" {
		return v
	}
	return fallback
}

// Finding is one impeccable design-audit issue on a built site, shown as a
// review checklist in /admin. Stored inline (plain JSON — no object storage).
type Finding struct {
	Antipattern string `json:"antipattern"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Severity    string `json:"severity"`
	File        string `json:"file"`
	Line        int    `json:"line"`
	Snippet     string `json:"snippet"`
}

// Project is a single customer request to have a website built.
type Project struct {
	ID               string
	UserID           string
	Name             string
	Brief            string // the customer's description of what they want
	Status           Status
	Questions        []string                   // clarifying questions from the intake step
	DesignOptions    []DesignOption             // suggested design directions from intake
	DesignBrief      string                     // the customer's chosen/stated design direction
	Answers          string                     // the customer's answers to those questions
	Plan             string                     // generated build plan (markdown; operator-facing)
	Spec             PlanSpec                   // machine-readable plan: pages, scope, content needs (customer UI)
	Verdict          Verdict                    // safety-gate outcome
	RejectReason     string                     // populated when Status == rejected
	PreviewURL       string                     // latest deployed preview link
	RepoURL          string                     // vestigial: GitHub mirroring was removed; kept to avoid a DB migration (always "")
	SnapshotKey      string                     // object-storage key of the workspace snapshot from the last successful build
	Screenshots      []Screenshot               // one per page of the deployed site (for /admin review)
	Findings         []Finding                  // impeccable design-audit findings from the last build (for /admin review)
	Critique         string                     // design critic's verdict on the preview screenshots ("SHIP" or "POLISH" + issues)
	Locale           string                     // customer's UI language at creation ("en"/"sv"/"ru"), for their emails
	ContentAnswers   map[string]string          // text the customer typed for text-kind content slots (slug → value)
	ContentRosters   map[string][]RosterEntry   // structured people for roster-kind slots (slug → members)
	PendingImages    map[string]ImageCandidates // AI images awaiting the customer's pick (slug → candidates)
	ImageGenCount    int                        // AI image generations run (cost cap)
	IterationsUsed   int                        // number of build passes consumed (1..MaxIterations)
	Paid             bool                       // payment settled — unlocks delivery (see MarkPaid)
	PaidAt           time.Time                  // when payment was recorded (zero = unpaid)
	PaidVia          string                     // how it was settled: "manual", "stripe", "legacy"; provenance for accounting
	StripeCustomerID string                     // Stripe customer, set when Checkout completes (for the billing portal)
	StripeSubID      string                     // Stripe subscription; the webhook matches lifecycle events back to the project through it
	ContentPending   bool                       // content was added/changed since the last build — offer a rebuild to apply it

	// Forge Pro change model: a paying subscriber's monthly change allowance.
	// ChangesThisPeriod counts changes used in the window starting at
	// ChangePeriodStart (advances monthly); overage past the allowance is billed.
	// DeliveredAt is set on the first delivery and stays — so a later self-serve
	// change goes live on accept without routing back through operator review.
	ChangesThisPeriod int
	ChangePeriodStart time.Time
	DeliveredAt       time.Time

	// Custom domain (see internal/orchestrator/domains.go). Zero value = none.
	DomainName       string         // the attached/purchased hostname, e.g. "acme.se"
	DomainStatus     DomainStatus   // lifecycle of the domain
	DomainKind       string         // "byod" | "purchased"
	DomainZoneID     string         // Cloudflare zone id (purchased domains)
	DomainIPv6       string         // dedicated apex IPv6 on the Fly app (allocate-once guard)
	DomainSubItemID  string         // Stripe subscription-item id for the monthly add-on
	DomainRecords    []DomainRecord // DNS records to show the customer (cached from the cert requirements)
	DomainCreatedAt  time.Time      // when the domain flow started (stuck-timeout clock)
	DomainVerifiedAt time.Time      // when it went active (guards one-time emails/billing)

	CreatedAt time.Time
	UpdatedAt time.Time
}

// EffectiveBrief is the brief enriched with the customer's clarifying answers
// and chosen design direction, as fed to the planner and safety gate.
func (p *Project) EffectiveBrief() string {
	b := p.Brief
	if p.Answers != "" {
		b += "\n\nClarifications from the customer:\n" + p.Answers
	}
	if p.DesignBrief != "" {
		b += "\n\nDesign direction chosen by the customer:\n" + p.DesignBrief
	}
	return b
}

// CanReiterate reports whether an UNPAID customer may refine the preview before
// subscribing — the free "try before you buy" passes (capped at MaxIterations).
// Paid subscribers use the monthly change model (CanRequestChange) instead.
func (p *Project) CanReiterate() bool {
	return !p.Paid && p.Status == StatusPreviewReady && p.IterationsUsed < MaxIterations
}

// CanRequestChange reports whether a paying subscriber may request a change now:
// the site is in a settled, live state (the preview, or an already-delivered
// site). There is no hard block — the monthly allowance decides free vs overage,
// billed, never refused (see ChangesLeft / the orchestrator's metering).
func (p *Project) CanRequestChange() bool {
	return p.Paid && (p.Status == StatusPreviewReady || p.Status == StatusDelivered)
}

// CanChange reports whether the customer may request a build change right now,
// by EITHER path: an unpaid preview refinement (CanReiterate) or a paid monthly
// change (CanRequestChange). The web handlers gate on this; the orchestrator's
// Reiterate picks the right path and enforces the per-path specifics.
func (p *Project) CanChange() bool {
	return p.CanReiterate() || p.CanRequestChange()
}

// currentChangePeriodStart returns the start of the monthly change window that
// contains `now`, advancing ChangePeriodStart in whole months. An unset start
// anchors the first window at `now`.
func (p *Project) currentChangePeriodStart(now time.Time) time.Time {
	if p.ChangePeriodStart.IsZero() {
		return now
	}
	start := p.ChangePeriodStart
	for !start.AddDate(0, 1, 0).After(now) {
		start = start.AddDate(0, 1, 0)
	}
	return start
}

// ChangesUsed returns changes consumed in the window containing `now` — 0 once a
// month has rolled over since the counter was last touched.
func (p *Project) ChangesUsed(now time.Time) int {
	if p.currentChangePeriodStart(now).After(p.ChangePeriodStart) {
		return 0
	}
	return p.ChangesThisPeriod
}

// ChangesLeft returns included changes remaining this month (never negative).
func (p *Project) ChangesLeft(now time.Time, perMonth int) int {
	if left := perMonth - p.ChangesUsed(now); left > 0 {
		return left
	}
	return 0
}

// RecordChange advances the change counter for `now`, rolling the monthly window
// if needed, and reports whether THIS change is overage (past the allowance).
func (p *Project) RecordChange(now time.Time, perMonth int) (overage bool) {
	if start := p.currentChangePeriodStart(now); start.After(p.ChangePeriodStart) {
		p.ChangePeriodStart = start
		p.ChangesThisPeriod = 0
	}
	overage = p.ChangesThisPeriod >= perMonth
	p.ChangesThisPeriod++
	return overage
}

// CanApprovePlan reports whether the customer is at the plan-approval gate: the
// plan is ready and the build hasn't started, waiting on their go-ahead.
func (p *Project) CanApprovePlan() bool {
	return p.Status == StatusAwaitingApproval
}

// TimelineStep is the furthest customer-facing milestone the project has
// reached, indexing TimelineSteps. Steps before it are done, this one is
// current. Terminal-bad states report the step they stopped at.
func (p *Project) TimelineStep() int {
	switch p.Status {
	case StatusCreated:
		return 0
	case StatusClarifying, StatusNeedsInput:
		return 1
	case StatusPlanning, StatusScreening, StatusAwaitingApproval, StatusEscalated, StatusRejected:
		return 2
	case StatusBuilding, StatusFailed:
		return 3
	case StatusPreviewReady, StatusAccepted, StatusExpired:
		return 4
	case StatusDelivered:
		return 5
	default:
		return 0
	}
}

// TimelineSteps are the six customer-facing milestones, by i18n key suffix
// (rendered as "timeline.<key>"). "review" is the deliberate human checkpoint.
var TimelineSteps = []string{"brief", "questions", "plan", "building", "review", "live"}

// CanAccept reports whether the customer may accept the current preview and
// send it to Rasmus for final review.
func (p *Project) CanAccept() bool {
	return p.Status == StatusPreviewReady
}

// HasDomain reports whether a domain flow is under way or complete.
func (p *Project) HasDomain() bool { return p.DomainStatus != DomainNone }

// CanAttachDomain reports whether the customer may start attaching or buying a
// domain: they've paid, a preview exists, and no domain is attached yet.
func (p *Project) CanAttachDomain() bool {
	return p.Paid && p.PreviewURL != "" && p.DomainStatus == DomainNone
}

// CanRetry reports whether a failed build can be retried. A build only reaches
// the terminal failed state when the *initial* build never produced a preview
// (a failed reiteration falls back to the still-live preview instead), so a
// retry re-runs that first build and consumes no change credit.
func (p *Project) CanRetry() bool {
	return p.Status == StatusFailed
}

// IterationsLeft is the number of reiterations still available to the customer.
func (p *Project) IterationsLeft() int {
	left := MaxIterations - p.IterationsUsed
	if left < 0 {
		return 0
	}
	return left
}

// Iteration records a single build pass within a project.
type Iteration struct {
	ID          string
	ProjectID   string
	Number      int    // 1 for the initial build, 2..MaxIterations for reiterations
	Prompt      string // empty for the initial build; the customer's change request otherwise
	PreviewURL  string
	Status      Status
	Log         string    // human-readable trace of what the build did
	MachineID   string    // Fly Machine running this build (for recovery/reaping)
	SessionID   string    // opencode session id — lets a restarted orchestrator re-attach to the still-running build
	SandboxAddr string    // sandbox opencode address (http://[ip]:port) — the re-attach target
	HeartbeatAt time.Time // last time the build reported progress
	Tokens      int       // model tokens the build agent consumed (cost visibility)
	// Which models produced this build — recorded at build start so model
	// experiments (make model-*) can be compared per build afterwards.
	ImplModel    string // the build/intake/gate model (LLM_MODEL)
	PlannerModel string // the planning model (PLANNER_LLM_MODEL)
	CreatedAt    time.Time
}

// Duration is how long the build ran, approximated by the last heartbeat.
func (it *Iteration) Duration() time.Duration {
	if it.HeartbeatAt.After(it.CreatedAt) {
		return it.HeartbeatAt.Sub(it.CreatedAt)
	}
	return 0
}

// Asset is a customer-uploaded file (photo, logo, content) for a project. The
// bytes live in object storage; this is the metadata row.
type Asset struct {
	ID          string
	ProjectID   string
	Key         string // object-storage key, e.g. projects/<id>/assets/<file>
	Filename    string
	ContentType string
	Description string // customer's one-liner: what this is / where it belongs ("our logo")
	Slot        string // content-slot slug this fills (from PlanSpec.ContentNeeded), "" = general upload
	Generated   bool   // true if this image was AI-generated (vs customer-uploaded)
	Size        int64
	CreatedAt   time.Time
}

// ErrNotFound is returned by stores when an entity does not exist.
var ErrNotFound = errors.New("not found")
