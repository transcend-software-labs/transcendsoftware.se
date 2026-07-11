package fly

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// certDetail is the Machines API CertificateDetail (the fields we consume).
// Returned by POST …/certificates/acme, GET …/certificates/{hostname}, and
// POST …/certificates/{hostname}/check.
type certDetail struct {
	Hostname   string `json:"hostname"`
	Configured bool   `json:"configured"`
	Status     string `json:"status"` // pending_validation | pending_ownership | active
	Validation struct {
		DNSConfigured          bool `json:"dns_configured"`
		ALPNConfigured         bool `json:"alpn_configured"`
		OwnershipTXTConfigured bool `json:"ownership_txt_configured"`
	} `json:"validation"`
	DNSRequirements struct {
		A             []string `json:"a"`    // apex A-record targets
		AAAA          []string `json:"aaaa"` // apex AAAA-record targets
		CNAME         string   `json:"cname"`
		ACMEChallenge struct {
			Name   string `json:"name"`   // "_acme-challenge.<host>"
			Target string `json:"target"` // "<host>.xxxx.flydns.net"
		} `json:"acme_challenge"`
		Ownership struct {
			Name     string `json:"name"` // "_fly-ownership.<host>"
			AppValue string `json:"app_value"`
			OrgValue string `json:"org_value"`
		} `json:"ownership"`
	} `json:"dns_requirements"`
}

// requirements flattens dns_requirements into the records the customer must set.
// Apex (A/AAAA present) → A + AAAA; subdomain → CNAME. Both always add the
// ACME-challenge CNAME, and the ownership TXT when Fly asks for one.
func (d certDetail) requirements() CertRequirements {
	r := d.DNSRequirements
	req := CertRequirements{Hostname: d.Hostname}
	req.IsApex = len(r.A) > 0 || len(r.AAAA) > 0
	for _, ip := range r.A {
		req.Records = append(req.Records, CertRecord{Type: "A", Name: d.Hostname, Value: ip})
	}
	for _, ip := range r.AAAA {
		req.Records = append(req.Records, CertRecord{Type: "AAAA", Name: d.Hostname, Value: ip})
	}
	if !req.IsApex && r.CNAME != "" {
		req.Records = append(req.Records, CertRecord{Type: "CNAME", Name: d.Hostname, Value: r.CNAME})
	}
	if r.ACMEChallenge.Name != "" {
		req.Records = append(req.Records, CertRecord{Type: "CNAME", Name: r.ACMEChallenge.Name, Value: r.ACMEChallenge.Target})
	}
	// Ownership TXT is only required when the hostname is contested; the
	// app-scoped value is the one to publish (verify in the live smoke test).
	if r.Ownership.Name != "" && r.Ownership.AppValue != "" {
		req.Records = append(req.Records, CertRecord{Type: "TXT", Name: r.Ownership.Name, Value: r.Ownership.AppValue})
	}
	return req
}

// AddCertificate requests an ACME certificate for hostname and returns the DNS
// records needed to validate it.
func (h *HTTP) AddCertificate(ctx context.Context, appName, hostname string) (CertRequirements, error) {
	var d certDetail
	if err := h.do(ctx, http.MethodPost, "/apps/"+appName+"/certificates/acme",
		map[string]any{"hostname": hostname}, &d); err != nil {
		return CertRequirements{}, err
	}
	if d.Hostname == "" {
		d.Hostname = hostname
	}
	return d.requirements(), nil
}

// CheckCertificate forces a validation re-check and reports the cert's state.
func (h *HTTP) CheckCertificate(ctx context.Context, appName, hostname string) (CertStatus, error) {
	var d certDetail
	if err := h.do(ctx, http.MethodPost,
		"/apps/"+appName+"/certificates/"+hostname+"/check", nil, &d); err != nil {
		return CertStatus{}, err
	}
	if d.Hostname == "" {
		d.Hostname = hostname
	}
	return CertStatus{
		Configured:    d.Configured,
		DNSConfigured: d.Validation.DNSConfigured,
		Status:        d.Status,
		Requirements:  d.requirements(),
	}, nil
}

// DeleteCertificate removes the hostname's certificate. A 404 (already gone) is
// treated as success so detach is idempotent.
func (h *HTTP) DeleteCertificate(ctx context.Context, appName, hostname string) error {
	err := h.do(ctx, http.MethodDelete, "/apps/"+appName+"/certificates/"+hostname, nil, nil)
	if err != nil && strings.Contains(err.Error(), "returned 404") {
		return nil
	}
	return err
}

// AllocateIPv6 allocates a dedicated IPv6 on the app (needed to serve an apex
// domain via A/AAAA) using the GraphQL allocateIpAddress mutation, and returns
// the address. IPv4 stays on Fly's shared address — dedicated v4 is billed and
// modern (SNI) clients don't need it.
func (h *HTTP) AllocateIPv6(ctx context.Context, appName string) (string, error) {
	const mutation = `mutation($input: AllocateIPAddressInput!){` +
		`allocateIpAddress(input:$input){ipAddress{address type}}}`
	vars := map[string]any{"input": map[string]any{"appId": appName, "type": "v6"}}
	var out struct {
		AllocateIPAddress struct {
			IPAddress struct {
				Address string `json:"address"`
				Type    string `json:"type"`
			} `json:"ipAddress"`
		} `json:"allocateIpAddress"`
	}
	if err := h.graphql(ctx, mutation, vars, &out); err != nil {
		return "", err
	}
	addr := out.AllocateIPAddress.IPAddress.Address
	if addr == "" {
		return "", fmt.Errorf("fly: allocateIpAddress returned no address")
	}
	return addr, nil
}
