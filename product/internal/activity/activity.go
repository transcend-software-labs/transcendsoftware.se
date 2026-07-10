// Package activity turns raw build-log lines into a small set of language-
// neutral action codes for the customer-facing status line ("Bygger dina
// sidor…" instead of "→ write internal/web/templates/maskiner.html").
//
// Display text NEVER originates here: the pipeline stores codes, and the web
// layer localizes them at render time (i18n keys "act.<code>"). That keeps
// every supported language a catalog entry, not a pipeline change.
//
// Codes are deliberately coarse. Phase 2 adds plan-driven page rules
// (building_page + the plan's per-locale page names); the Tracker and rule
// shape here are built to take that without rework.
package activity

import (
	"regexp"
	"sync"
	"time"
)

// Code is a language-neutral build activity, rendered via i18n key "act.<code>".
type Code string

const (
	Preparing    Code = "preparing"     // sandbox/template/workspace setup
	Building     Code = "building"      // writing pages and application code
	Styling      Code = "styling"       // CSS / design work
	Database     Code = "database"      // schema and migrations
	Testing      Code = "testing"       // tests and browser checks
	Reviewing    Code = "reviewing"     // design audit, screenshots
	Deploying    Code = "deploying"     // publishing the preview
	Working      Code = "working"       // fallback while events flow
	TakingLonger Code = "taking_longer" // stall: no events for a while
)

// rules are evaluated top-down against a log line; first match wins. Lines are
// our own formats — toolLine's "→ write <path>" / "→ bash: <cmd>" plus the
// builder's human emits — so the patterns key on those, not on free text.
var rules = []struct {
	re   *regexp.Regexp
	code Code
}{
	{regexp.MustCompile(`fly deploy|flyctl deploy|Deploying|fly\.toml`), Deploying},
	{regexp.MustCompile(`audit\.js|impeccable|[Ss]creenshot|Design audit`), Reviewing},
	{regexp.MustCompile(`_test\.go|go test|flow\.js|smoke\.js|[Vv]erif`), Testing},
	{regexp.MustCompile(`migrations/|\.sql`), Database},
	{regexp.MustCompile(`\.css`), Styling},
	{regexp.MustCompile(`templates/|\.html|\.go|handlers|[Ss]caffold`), Building},
	{regexp.MustCompile(`[Ss]andbox|starter app|session started|[Cc]loning|[Ww]orkspace|[Ii]nstalling`), Preparing},
}

// classify maps a log line to a code; ok=false means the line says nothing
// about what's happening (it still counts as a liveness signal).
func classify(line string) (Code, bool) {
	for _, r := range rules {
		if r.re.MatchString(line) {
			return r.code, true
		}
	}
	return "", false
}

// Debounce: a status line that flips several times a second reads as glitchy.
// A promoted code is held at least minHold before a different one replaces it.
const minHold = 12 * time.Second

// stallAfter with no events at all turns the status into TakingLonger — the
// honest state when a build has gone quiet.
const stallAfter = 4 * time.Minute

type state struct {
	code      Code
	promoted  time.Time // when code was last changed
	lastEvent time.Time // any event, matched or not
}

// Tracker keeps the debounced current activity per project. In-memory only:
// activity is ephemeral progress, the log itself is the durable record.
type Tracker struct {
	mu  sync.Mutex
	cur map[string]*state
	now func() time.Time // injectable for tests
}

func NewTracker() *Tracker {
	return &Tracker{cur: map[string]*state{}, now: time.Now}
}

// Observe feeds one log line. Unmatched lines only bump liveness.
func (t *Tracker) Observe(projectID, line string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	s := t.cur[projectID]
	if s == nil {
		s = &state{code: Working, promoted: now}
		t.cur[projectID] = s
	}
	s.lastEvent = now
	code, ok := classify(line)
	if !ok || code == s.code {
		return
	}
	if now.Sub(s.promoted) >= minHold || s.code == Working {
		s.code, s.promoted = code, now
	}
}

// Current returns the activity code to show for a project, or "" when no
// build is being tracked. A long silence reports TakingLonger.
func (t *Tracker) Current(projectID string) Code {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.cur[projectID]
	if s == nil {
		return ""
	}
	if t.now().Sub(s.lastEvent) >= stallAfter {
		return TakingLonger
	}
	return s.code
}

// Clear drops a project's state — call when its build finishes or fails.
func (t *Tracker) Clear(projectID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.cur, projectID)
}
