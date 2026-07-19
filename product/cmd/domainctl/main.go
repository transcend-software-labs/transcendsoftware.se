// Command domainctl is the operator's name.com diagnosis tool: it exercises
// the EXACT client code Forge uses (internal/namecom), one step at a time,
// with every outcome printed — so a failure can be pinned to our code, our
// account, or the registrar, and the output pasted to support as evidence.
//
// Read-only by default. Registering costs real money in production; against
// the sandbox (https://api.dev.name.com — the default in dev mode) it is
// simulated and free. It needs the explicit -register flag either way.
//
//	NAME_DOT_COM_USERNAME=user-test NAME_DOT_COM_API_KEY=... go run ./cmd/domainctl                       # hello + account survey
//	... go run ./cmd/domainctl -domain forgetest123.com                                                   # + state, availability, records
//	... go run ./cmd/domainctl -domain forgetest123.com -register                                         # registration attempt
//	... go run ./cmd/domainctl -domain forgetest123.com -ensure-dns 66.241.125.213                        # write an apex A record
//	... go run ./cmd/domainctl -domain forgetest123.com -autorenew=off                                    # toggle auto-renew
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/config"
	"github.com/transcend-software-labs/rasmus-ai/internal/namecom"
	"github.com/transcend-software-labs/rasmus-ai/internal/registrar"
)

func main() {
	domain := flag.String("domain", "", "domain to inspect (e.g. forgetest123.com)")
	register := flag.Bool("register", false, "actually attempt registration (REAL MONEY in prod; requires -domain)")
	ensureDNS := flag.String("ensure-dns", "", "write an apex A record with this IP to the domain's zone (requires -domain)")
	autorenew := flag.String("autorenew", "", "set auto-renew on|off (requires -domain)")
	var ensures ensureList
	flag.Var(&ensures, "ensure", `DNS record to ensure as "TYPE HOST VALUE" (repeatable; HOST @ = apex, relative or FQDN) — the generic form of what provisioning writes`)
	flag.Parse()

	cfg := config.Load()
	if !cfg.NameComEnabled() {
		fatal("NAME_DOT_COM_USERNAME and NAME_DOT_COM_API_KEY must be set")
	}
	fmt.Printf("== domainctl: name.com @ %s, user %s ==\n", cfg.NameComAPIURL, cfg.NameComUsername)
	c := namecom.New(cfg.NameComAPIURL, cfg.NameComUsername, cfg.NameComAPIKey, cfg.SekPerUSD, cfg.DomainMarkupPct)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// 1) Hello — proves connectivity + credentials.
	user, server, err := c.Hello(ctx)
	if err != nil {
		fatal("hello failed (credentials? sandbox needs the -test username + its own token): %v", err)
	}
	fmt.Printf("   hello OK — authenticated as %q on %s\n", user, server)

	// 2) Account survey.
	fmt.Println("\n-- account domains --")
	ds, err := c.ListDomains(ctx)
	if err != nil {
		fatal("list domains: %v", err)
	}
	if len(ds) == 0 {
		fmt.Println("   (none)")
	}
	for _, d := range ds {
		fmt.Printf("   %-30s expires=%s autorenew=%v\n", d.Name, d.Expire, d.Autorenew)
	}

	if *domain == "" {
		return
	}

	// 3) The target domain: in-account state, availability + price, records.
	fmt.Printf("\n-- %s: state in account --\n", *domain)
	st, err := c.RegistrationStatus(ctx, *domain)
	switch {
	case err != nil:
		fmt.Printf("   status ERROR: %v\n", err)
	case st == registrar.StateSucceeded:
		fmt.Println("   registered in this account")
		if exp, aerr := c.DomainExpiry(ctx, *domain); aerr == nil && !exp.IsZero() {
			fmt.Printf("   expires: %s\n", exp.Format("2006-01-02"))
		}
		if ar, aerr := c.AutorenewEnabled(ctx, *domain); aerr == nil {
			fmt.Printf("   autorenew: %v\n", ar)
		}
	default:
		fmt.Println("   not in the account")
	}

	fmt.Printf("\n-- %s: availability + price --\n", *domain)
	offers, err := c.CheckDomains(ctx, []string{*domain})
	if err != nil {
		fmt.Printf("   availability ERROR: %v\n", err)
	}
	for _, o := range offers {
		fmt.Printf("   %s registrable=%v price=%.2f %s (1y)\n", o.Name, o.Registrable, o.Price, o.Currency)
	}

	if st == registrar.StateSucceeded {
		fmt.Printf("\n-- %s: DNS records --\n", *domain)
		recs, err := c.Records(ctx, *domain)
		if err != nil {
			fmt.Printf("   records ERROR: %v\n", err)
		}
		if len(recs) == 0 {
			fmt.Println("   (none)")
		}
		for _, r := range recs {
			host := r.Name
			if host == "" {
				host = "@"
			}
			fmt.Printf("   %-6s %-25s %s\n", r.Type, host, r.Content)
		}
	}

	// 4) Optional writes.
	if *register {
		fmt.Printf("\n-- %s: REGISTRATION ATTEMPT (the real code path) --\n", *domain)
		state, err := c.RegisterDomain(ctx, *domain)
		if err != nil {
			fmt.Printf("   register FAILED: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("   register OK — state: %s\n", state)
		if exp, err := c.DomainExpiry(ctx, *domain); err == nil && !exp.IsZero() {
			fmt.Printf("   expires: %s\n", exp.Format("2006-01-02"))
		}
	}

	if *ensureDNS != "" {
		ensures = append(ensures, ensureSpec{Type: "A", Host: "@", Value: *ensureDNS})
	}
	for _, e := range ensures {
		name := e.Host
		if name == "@" {
			name = *domain
		}
		fmt.Printf("\n-- %s: ensure %s %s → %s --\n", *domain, e.Type, e.Host, e.Value)
		if err := c.EnsureDNSRecord(ctx, *domain, registrar.Record{Type: e.Type, Name: name, Content: e.Value}); err != nil {
			fmt.Printf("   ensure FAILED: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("   ensure OK (created or already present)")
	}

	if *autorenew != "" {
		on := *autorenew == "on"
		fmt.Printf("\n-- %s: set auto-renew %v --\n", *domain, on)
		if err := c.SetAutoRenew(ctx, *domain, on); err != nil {
			fmt.Printf("   autorenew FAILED: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("   autorenew OK")
	}
}

// ensureSpec is one -ensure record: TYPE HOST VALUE.
type ensureSpec struct{ Type, Host, Value string }

// ensureList collects repeated -ensure flags.
type ensureList []ensureSpec

func (l *ensureList) String() string { return fmt.Sprintf("%v", []ensureSpec(*l)) }
func (l *ensureList) Set(s string) error {
	f := strings.Fields(s)
	if len(f) != 3 {
		return fmt.Errorf(`want "TYPE HOST VALUE", got %q`, s)
	}
	*l = append(*l, ensureSpec{Type: strings.ToUpper(f[0]), Host: f[1], Value: f[2]})
	return nil
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "domainctl: "+format+"\n", args...)
	os.Exit(1)
}
