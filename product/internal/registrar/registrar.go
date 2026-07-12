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

// Offer is a domain we can (or can't) register, with its prices in the
// provider's currency (USD for Cloudflare, SEK for Hostup). Prices are never
// shown to customers — they only gate buyability against the configured cap.
type Offer struct {
	Name        string
	Registrable bool
	Premium     bool
	Price       float64 // one-year registration cost
	Renewal     float64 // yearly renewal cost (0 = not reported)
	Currency    string
}

// Buyable reports whether the offer can be sold self-serve under cap: it must
// be registrable, carry a real price, and neither the registration nor the
// renewal may exceed the cap — registrars often discount the first year, and a
// cheap registration with a pricey renewal would otherwise outrun the flat
// monthly add-on that funds it. The same check gates both the search results
// and the server-side buy guard, for every provider.
func (o Offer) Buyable(cap float64) bool {
	return o.Registrable && o.Price > 0 && o.Price <= cap && o.Renewal <= cap
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
