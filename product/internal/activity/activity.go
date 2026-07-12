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

	"github.com/transcend-software-labs/rasmus-ai/internal/project"
)

// Code is a language-neutral build activity, rendered via i18n key "act.<code>".
type Code string

const (
	Preparing    Code = "preparing"     // sandbox/template/workspace setup
	Building     Code = "building"      // writing pages (templates/HTML)
	Backend      Code = "backend"       // Go application code
	Interactive  Code = "interactive"   // client-side JS/TS touches
	Images       Code = "images"        // placing images, icons, graphics
	Styling      Code = "styling"       // CSS / design work
	Database     Code = "database"      // schema and migrations
	Dependencies Code = "dependencies"  // fetching modules/packages
	Compiling    Code = "compiling"     // go build/vet — assembling the parts
	Testing      Code = "testing"       // tests and browser checks
	Reviewing    Code = "reviewing"     // design audit, screenshots
	Deploying    Code = "deploying"     // publishing the preview
	Working      Code = "working"       // fallback while events flow
	TakingLonger Code = "taking_longer" // stall: no events for a while
)

// rules are evaluated top-down against a log line; first match wins. Lines are
// our own formats — toolLine's "→ write <path>" / "→ bash: <cmd>" plus the
// builder's human emits — so the patterns key on file extensions and command
// names, not free text. Order matters: test/review .js files must win before
// the generic Interactive rule, and the file-type rules must all come before
// Preparing (which must NOT match "workspace" — every sandbox path starts with
// /workspace/, and matching it turned any unclassified file line into a
// permanent "preparing", which is exactly the monotony customers noticed).
var rules = []struct {
	re   *regexp.Regexp
	code Code
}{
	{regexp.MustCompile(`fly deploy|flyctl|fly\.toml|Dockerfile|[Dd]eploying`), Deploying},
	{regexp.MustCompile(`audit\.js|impeccable|[Ss]creenshot|crawl\.js|Design audit`), Reviewing},
	{regexp.MustCompile(`_test\.go|go test|flow\.js|smoke\.js|[Vv]erif|healthz`), Testing},
	// Dependencies before Database: "go get …/go-sqlite3" is fetching a module,
	// not schema work, even though the package name mentions sqlite.
	{regexp.MustCompile(`go\.(mod|sum)|go get|npm (i|install|ci)|pnpm|yarn add`), Dependencies},
	{regexp.MustCompile(`migrations/|\.sql\b|[Ss]qlite`), Database},
	{regexp.MustCompile(`\.css\b|[Tt]ailwind|[Ff]ont|[Pp]alette|[Ss]tyling`), Styling},
	{regexp.MustCompile(`\.(png|jpe?g|webp|svg|gif|ico)\b|[Ff]avicon|images?/`), Images},
	{regexp.MustCompile(`\.[tj]sx?\b`), Interactive},
	{regexp.MustCompile(`go (build|vet|run)|gofmt|[Cc]ompil`), Compiling},
	{regexp.MustCompile(`templates/|\.html\b|[Ss]caffold`), Building},
	{regexp.MustCompile(`\.go\b|handlers`), Backend},
	{regexp.MustCompile(`[Ss]andbox|starter app|session started|[Cc]loning|[Ii]nstalling`), Preparing},
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
// Short enough that the finer-grained codes actually cycle during a build.
const minHold = 8 * time.Second

// stallAfter with no events at all turns the status into TakingLonger — the
// honest state when a build has gone quiet.
const stallAfter = 4 * time.Minute

// pageMarkerRe is how the build agent authoritatively reports a finished page:
// it writes a line "FORGE_PAGE_DONE: <slug>" (see the build prompt). Heuristics
// mark "building"; only the agent knows "done".
var pageMarkerRe = regexp.MustCompile(`FORGE_PAGE_DONE:\s*([a-z0-9_-]+)`)

const (
	pagePending  = "pending"
	pageBuilding = "building"
	pageDone     = "done"
)

// PageStatus is one page's checklist row for the customer hero.
type PageStatus struct {
	Slug   string
	Names  map[string]string
	Status string // pending | building | done
}

// Name resolves the page's display label in lang (en fallback, then slug).
func (p PageStatus) Name(lang string) string {
	if v := p.Names[lang]; v != "" {
		return v
	}
	if v := p.Names["en"]; v != "" {
		return v
	}
	return p.Slug
}

type pageState struct {
	slug   string
	names  map[string]string
	re     *regexp.Regexp // matches this page's path hints in a log line
	status string
}

type state struct {
	code      Code
	promoted  time.Time // when code was last changed
	lastEvent time.Time // any event, matched or not
	pages     []*pageState
	building  string // slug of the page most recently seen building
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

// SetPages primes per-page progress from the plan for a project's build. Path
// hints compile into one matcher per page; a line touching those paths marks
// the page "building". Called at build start; safe to call with no pages.
func (t *Tracker) SetPages(projectID string, pages []project.PlanPage) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.cur[projectID]
	if s == nil {
		s = &state{code: Working, promoted: t.now(), lastEvent: t.now()}
		t.cur[projectID] = s
	}
	s.pages = nil
	s.building = ""
	for _, pg := range pages {
		ps := &pageState{slug: pg.Slug, names: pg.Names, status: pagePending}
		if re := pathRegex(pg.Paths); re != nil {
			ps.re = re
		}
		s.pages = append(s.pages, ps)
	}
}

// pathRegex builds a case-insensitive alternation of the (regex-quoted) path
// hints, or nil when there are none.
func pathRegex(paths []string) *regexp.Regexp {
	var quoted []string
	for _, p := range paths {
		if p = regexp.QuoteMeta(p); p != "" {
			quoted = append(quoted, p)
		}
	}
	if len(quoted) == 0 {
		return nil
	}
	re, err := regexp.Compile(`(?i)(` + join(quoted, "|") + `)`)
	if err != nil {
		return nil
	}
	return re
}

func join(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
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

	// Authoritative page completion from the agent's own marker.
	if m := pageMarkerRe.FindStringSubmatch(line); m != nil {
		for _, pg := range s.pages {
			if pg.slug == m[1] {
				pg.status = pageDone
				if s.building == pg.slug {
					s.building = ""
				}
			}
		}
	} else {
		// Heuristic: a line touching a page's paths means it's being built.
		for _, pg := range s.pages {
			if pg.status != pageDone && pg.re != nil && pg.re.MatchString(line) {
				pg.status = pageBuilding
				s.building = pg.slug
				break
			}
		}
	}

	code, ok := classify(line)
	if !ok || code == s.code {
		return
	}
	if now.Sub(s.promoted) >= minHold || s.code == Working {
		s.code, s.promoted = code, now
	}
}

// Pages returns a snapshot of the page checklist, or nil when none is tracked.
func (t *Tracker) Pages(projectID string) []PageStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.cur[projectID]
	if s == nil || len(s.pages) == 0 {
		return nil
	}
	out := make([]PageStatus, len(s.pages))
	for i, pg := range s.pages {
		out[i] = PageStatus{Slug: pg.slug, Names: pg.names, Status: pg.status}
	}
	return out
}

// Building returns the page currently being built (for a "Building X…" status
// line), if any is tracked and actively building.
func (t *Tracker) Building(projectID string) (PageStatus, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.cur[projectID]
	if s == nil || s.building == "" {
		return PageStatus{}, false
	}
	for _, pg := range s.pages {
		if pg.slug == s.building && pg.status == pageBuilding {
			return PageStatus{Slug: pg.slug, Names: pg.names, Status: pg.status}, true
		}
	}
	return PageStatus{}, false
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
