// Package registrar holds the provider-neutral types shared by domain
// registrars. The orchestrator drives domain purchase + DNS through its
// DomainRegistrar interface (internal/orchestrator/domains.go); both
// internal/cloudflare and internal/hostup implement it with these types, so
// the provider is swappable at wiring time (cmd/server/main.go) and the two
// can be compared side by side.
package registrar

// Registration workflow states reported by RegisterDomain / RegistrationStatus.
// Cloudflare's Registrar API uses these verbatim; other providers map onto them.
const (
	StateSucceeded      = "succeeded"
	StatePending        = "pending"
	StateInProgress     = "in_progress"
	StateActionRequired = "action_required"
	StateBlocked        = "blocked"
	StateFailed         = "failed"
)

// Offer is a domain we can (or can't) register, with its one-year registration
// price in the provider's currency (USD for Cloudflare, SEK for Hostup). The
// price is never shown to customers — it only gates buyability against the
// configured cap.
type Offer struct {
	Name        string
	Registrable bool
	Premium     bool
	Price       float64
	Currency    string
}

// Record is one DNS record to ensure in a zone. Name may be a FQDN — providers
// whose APIs want zone-relative names convert internally. Proxied exists for
// Cloudflare (where it must always be false — the orange-cloud proxy breaks
// Fly's ACME + TLS); other providers ignore it.
type Record struct {
	ID      string
	Type    string // A | AAAA | CNAME | TXT
	Name    string
	Content string
	TTL     int
	Proxied bool
}
