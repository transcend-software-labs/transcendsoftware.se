package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/id"
	"github.com/transcend-software-labs/rasmus-ai/internal/store"
)

func (s *Server) handleTerms(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusOK, "terms", s.view(r, s.t(r, "terms.title"), nil))
}

func (s *Server) handlePrivacy(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusOK, "privacy", s.view(r, s.t(r, "privacy.title"), nil))
}

type withdrawalView struct {
	Reference string
	Error     string
}

func (s *Server) handleWithdrawalForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusOK, "withdraw", s.view(r, s.t(r, "withdraw.title"), withdrawalView{}))
}

func (s *Server) handleWithdrawal(w http.ResponseWriter, r *http.Request) {
	if !sameOriginPost(r) {
		http.Error(w, "cross-site request rejected", http.StatusForbidden)
		return
	}
	email := strings.ToLower(strings.TrimSpace(r.FormValue("email")))
	projectID := strings.TrimSpace(r.FormValue("project_id"))
	if !strings.Contains(email, "@") || len(email) > 200 || len(projectID) > 100 || r.FormValue("confirm") != "yes" {
		v := s.view(r, s.t(r, "withdraw.title"), withdrawalView{Error: s.t(r, "withdraw.invalid")})
		s.render(w, http.StatusBadRequest, "withdraw", v)
		return
	}
	if !s.allowAuth(r, "withdraw", email, 8, 3, 24*time.Hour) {
		v := s.view(r, s.t(r, "withdraw.title"), withdrawalView{Error: s.t(r, "flash.try_later")})
		s.render(w, http.StatusTooManyRequests, "withdraw", v)
		return
	}
	now := time.Now().UTC()
	request := store.WithdrawalRequest{ID: id.New(), Email: email, ProjectID: projectID, CreatedAt: now}
	if err := s.store.RecordWithdrawalRequest(r.Context(), request); err != nil {
		s.log.Error("record withdrawal", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	body := fmt.Sprintf(s.t(r, "withdraw.email.body"), request.ID, now.Format(time.RFC3339), projectID)
	mailErr := s.notifier.Send(r.Context(), email, s.t(r, "withdraw.email.subject"), body)
	if s.cfg.AdminEmail != "" {
		operatorBody := fmt.Sprintf("Withdrawal request %s\nEmail: %s\nProject/reference: %s\nReceived: %s",
			request.ID, email, projectID, now.Format(time.RFC3339))
		if err := s.notifier.Send(r.Context(), s.cfg.AdminEmail, "Forge: withdrawal request", operatorBody); err != nil {
			s.log.Error("notify withdrawal operator", "request", request.ID, "err", err)
		}
	}
	data := withdrawalView{Reference: request.ID}
	status := http.StatusOK
	if mailErr != nil {
		s.log.Error("withdrawal confirmation email", "request", request.ID, "err", mailErr)
		data.Error = s.t(r, "withdraw.email_failed")
		status = http.StatusBadGateway
	}
	s.render(w, status, "withdraw", s.view(r, s.t(r, "withdraw.title"), data))
}

// sameOriginPost prevents another website from silently filing a legally
// significant withdrawal request in a visitor's name. Normal HTML form POSTs
// send Origin; Referer is a compatibility fallback for older clients.
func sameOriginPost(r *http.Request) bool {
	raw := strings.TrimSpace(r.Header.Get("Origin"))
	if raw == "" {
		raw = strings.TrimSpace(r.Header.Get("Referer"))
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return strings.EqualFold(u.Scheme, scheme) && strings.EqualFold(u.Host, r.Host)
}
