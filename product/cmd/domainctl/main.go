// Command domainctl is the operator's GleSYS diagnosis tool: it exercises the
// EXACT client code Forge uses (internal/glesys), one step at a time, with
// every request/outcome printed — so a failure can be pinned to our code, our
// account, or GleSYS, and the output pasted to GleSYS support as evidence.
//
// Read-only by default. Registering costs real money and needs the explicit
// -register flag.
//
//	GLESYS_PROJECT_ID=CLxxxxx GLESYS_API_KEY=... go run ./cmd/domainctl            # account survey
//	... go run ./cmd/domainctl -domain ippfnotti.nu                                # + availability, state, records
//	... go run ./cmd/domainctl -domain ippfnotti.nu -register                      # REAL registration attempt
//	... go run ./cmd/domainctl -domain ippfnotti.nu -ensure-dns 1.2.3.4            # write an apex A record (config test)
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"flag"

	"github.com/transcend-software-labs/rasmus-ai/internal/config"
	"github.com/transcend-software-labs/rasmus-ai/internal/glesys"
	"github.com/transcend-software-labs/rasmus-ai/internal/registrar"
)

func main() {
	domain := flag.String("domain", "", "domain to inspect (e.g. ippfnotti.nu)")
	register := flag.Bool("register", false, "actually attempt registration (REAL MONEY; requires -domain)")
	ensureDNS := flag.String("ensure-dns", "", "write an apex A record with this IP to the domain's zone (requires -domain)")
	deleteZone := flag.Bool("delete-zone", false, "delete the domain's DNS zone — refused for registered domains (requires -domain)")
	flag.Parse()

	cfg := config.Load()
	if cfg.GlesysProjectID == "" || cfg.GlesysAPIKey == "" {
		fatal("GLESYS_PROJECT_ID and GLESYS_API_KEY must be set")
	}
	fmt.Printf("== domainctl: project %s, registrant complete: %v ==\n", cfg.GlesysProjectID, cfg.GlesysRegistrant.Complete())
	c := glesys.New(cfg.GlesysProjectID, cfg.GlesysAPIKey, glesys.Registrant(cfg.GlesysRegistrant))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// 1) Account survey — proves auth + shows every domain and its registrar
	// state ("" = DNS-only zone, never registered / not registered here).
	fmt.Println("\n-- account domains (domain/list) --")
	ds, err := c.ListDomains(ctx)
	if err != nil {
		fatal("list domains failed (auth? IP allowlist on the API key?): %v", err)
	}
	if len(ds) == 0 {
		fmt.Println("   (none)")
	}
	for _, d := range ds {
		state := d.State
		if state == "" {
			state = "DNS-ONLY (no registrar state)"
		}
		fmt.Printf("   %-30s state=%s", d.Name, state)
		if d.Expire != "" {
			fmt.Printf(" expires=%s", d.Expire)
		}
		fmt.Println()
	}

	if *domain == "" {
		return
	}

	// 2) The target domain: in-account state, availability + price, records.
	fmt.Printf("\n-- %s: state in account (domain/details) --\n", *domain)
	state, inAccount, err := c.DomainState(ctx, *domain)
	switch {
	case err != nil:
		fmt.Printf("   details ERROR: %v\n", err)
	case !inAccount:
		fmt.Println("   not in the account at all (details 404)")
	case state == "":
		fmt.Println("   in the account as a DNS zone only — NOT registered (registrarinfo None)")
	default:
		fmt.Printf("   registrar state: %s\n", state)
	}

	fmt.Printf("\n-- %s: availability + price (domain/available) --\n", *domain)
	offers, err := c.CheckDomains(ctx, []string{*domain})
	if err != nil {
		fmt.Printf("   availability ERROR: %v\n", err)
	}
	for _, o := range offers {
		fmt.Printf("   %s registrable=%v price=%.2f %s\n", o.Name, o.Registrable, o.Price, o.Currency)
	}

	if inAccount {
		fmt.Printf("\n-- %s: DNS records (domain/listrecords) --\n", *domain)
		recs, err := c.Records(ctx, *domain)
		if err != nil {
			fmt.Printf("   records ERROR: %v\n", err)
		}
		for _, r := range recs {
			fmt.Printf("   %-6s %-25s %s\n", r.Type, r.Name, r.Content)
		}
	}

	// 3) Optional writes.
	if *deleteZone {
		fmt.Printf("\n-- %s: DELETE DNS ZONE (domain/delete; refused for registered domains) --\n", *domain)
		if err := c.DeleteDomainZone(ctx, *domain); err != nil {
			fmt.Printf("   delete FAILED: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("   delete OK")
	}

	if *register {
		fmt.Printf("\n-- %s: REGISTRATION ATTEMPT (details→add→register, the real code path) --\n", *domain)
		st, err := c.RegisterDomain(ctx, *domain)
		if err != nil {
			fmt.Printf("   register FAILED: %v\n", err)
			fmt.Println("\n   If this is a 403 \"not allowed to register any more domains\":")
			fmt.Println("   the block is on the GleSYS ACCOUNT (their side) — this exact request,")
			fmt.Println("   params and auth otherwise succeed. Paste this output to GleSYS support.")
			os.Exit(1)
		}
		fmt.Printf("   register OK — workflow state: %s\n", st)
		st2, in2, _ := c.DomainState(ctx, *domain)
		fmt.Printf("   post-register details: inAccount=%v state=%q\n", in2, st2)
	}

	if *ensureDNS != "" {
		fmt.Printf("\n-- %s: ensure apex A %s (domain/addrecord) --\n", *domain, *ensureDNS)
		err := c.EnsureDNSRecord(ctx, *domain, registrar.Record{Type: "A", Name: *domain, Content: *ensureDNS})
		if err != nil {
			fmt.Printf("   ensure FAILED: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("   ensure OK (created or already present)")
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "domainctl: "+format+"\n", args...)
	os.Exit(1)
}
