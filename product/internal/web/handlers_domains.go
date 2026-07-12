package web

import (
	"errors"
	"net/http"
	"strings"

	"github.com/transcend-software-labs/rasmus-ai/internal/orchestrator"
	"github.com/transcend-software-labs/rasmus-ai/internal/user"
	"github.com/transcend-software-labs/rasmus-ai/internal/web/i18n"
)

// handleDomainAttach starts the BYOD flow for the customer's own hostname.
func (s *Server) handleDomainAttach(w http.ResponseWriter, r *http.Request, u *user.User) {
	p, ok := s.ownedProject(w, r, u)
	if !ok {
		return
	}
	host := strings.TrimSpace(r.FormValue("host"))
	code := "attached"
	if err := s.orch.AttachDomain(r.Context(), p.ID, host); err != nil {
		s.log.Error("domain attach", "project", p.ID, "err", err)
		code = domainRedirectCode(err)
	}
	http.Redirect(w, r, "/projects/"+p.ID+"?domain="+code, http.StatusSeeOther)
}

// handleDomainVerify re-checks a pending domain now.
func (s *Server) handleDomainVerify(w http.ResponseWriter, r *http.Request, u *user.User) {
	p, ok := s.ownedProject(w, r, u)
	if !ok {
		return
	}
	if err := s.orch.VerifyDomain(r.Context(), p.ID); err != nil {
		s.log.Error("domain verify", "project", p.ID, "err", err)
	}
	http.Redirect(w, r, "/projects/"+p.ID+"?domain=checking", http.StatusSeeOther)
}

// handleDomainBuy registers a domain the customer picked from search. The buy
// button carries a confirm dialog; the server re-checks price/availability
// authoritatively (the flat monthly add-on is what the customer actually pays).
func (s *Server) handleDomainBuy(w http.ResponseWriter, r *http.Request, u *user.User) {
	p, ok := s.ownedProject(w, r, u)
	if !ok {
		return
	}
	domain := strings.TrimSpace(r.FormValue("domain"))
	if r.FormValue("ack") != "1" { // confirm-dialog acknowledgement guard
		http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
		return
	}
	code := "buying"
	if err := s.orch.BuyDomain(r.Context(), p.ID, domain); err != nil {
		s.log.Error("domain buy", "project", p.ID, "err", err)
		code = domainRedirectCode(err)
	}
	http.Redirect(w, r, "/projects/"+p.ID+"?domain="+code, http.StatusSeeOther)
}

// handleDomainSearch renders an htmx fragment of registrable domains for a query.
func (s *Server) handleDomainSearch(w http.ResponseWriter, r *http.Request, u *user.User) {
	p, ok := s.ownedProject(w, r, u)
	if !ok {
		return
	}
	lang := s.lang(r)
	data := map[string]any{"PID": p.ID}
	if !s.orch.DomainBuyEnabled() || !p.CanAttachDomain() {
		s.renderFragment(w, r, "domain_results", data)
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(q) < 2 {
		s.renderFragment(w, r, "domain_results", data)
		return
	}
	offers, err := s.orch.SearchDomains(r.Context(), q)
	if err != nil {
		s.log.Error("domain search", "err", err)
		data["Error"] = i18n.T(lang, "domain.search_error")
		s.renderFragment(w, r, "domain_results", data)
		return
	}
	// Show only domains the customer can actually buy — skip taken, premium,
	// over-cap or unsupported ones rather than listing them as unavailable. The
	// wholesale price gates this but is never shown (they pay the flat add-on).
	cap := s.orch.MaxDomainUSD()
	type result struct{ Name string }
	var results []result
	for _, o := range offers {
		if o.Registrable && o.Price > 0 && o.Price <= cap {
			results = append(results, result{Name: o.Name})
		}
	}
	data["Results"] = results
	data["None"] = len(results) == 0
	s.renderFragment(w, r, "domain_results", data)
}

// handleDomainDetach lets the customer remove their domain.
func (s *Server) handleDomainDetach(w http.ResponseWriter, r *http.Request, u *user.User) {
	p, ok := s.ownedProject(w, r, u)
	if !ok {
		return
	}
	if err := s.orch.DetachDomain(r.Context(), p.ID); err != nil {
		s.log.Error("domain detach", "project", p.ID, "err", err)
	}
	http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
}

// handleAdminDomainDetach is the operator's troubleshoot action.
func (s *Server) handleAdminDomainDetach(w http.ResponseWriter, r *http.Request, _ *user.User) {
	id := r.PathValue("id")
	if err := s.orch.DetachDomain(r.Context(), id); err != nil {
		s.log.Error("admin domain detach", "project", id, "err", err)
	}
	http.Redirect(w, r, "/admin/projects/"+id, http.StatusSeeOther)
}

// renderFragment executes a named template fragment for an htmx swap, supplying
// Lang/CSRF so it can translate and post safely.
func (s *Server) renderFragment(w http.ResponseWriter, r *http.Request, name string, data map[string]any) {
	full := map[string]any{"Lang": s.lang(r), "CSRF": s.csrfToken(r), "Data": data}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, full); err != nil {
		s.log.Error("render fragment", "name", name, "err", err)
	}
}

// domainRedirectCode maps an orchestrator domain error to a ?domain=<code> so
// handleProject can show the right localized flash.
func domainRedirectCode(err error) string {
	switch {
	case errors.Is(err, orchestrator.ErrBadHostname):
		return "invalid"
	case errors.Is(err, orchestrator.ErrDomainTooPricey):
		return "toopricey"
	case errors.Is(err, orchestrator.ErrNotRegistrable):
		return "unavailable"
	case errors.Is(err, orchestrator.ErrDomainExists):
		return "exists"
	default:
		return "error"
	}
}
