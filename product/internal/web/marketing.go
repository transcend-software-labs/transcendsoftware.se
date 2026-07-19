package web

import (
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/transcend-software-labs/rasmus-ai/internal/store"
)

const maxCampaignValueRunes = 80

type campaignAttribution struct {
	Source   string
	Medium   string
	Campaign string
}

func cleanCampaignValue(value string) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.TrimSpace(strings.ToValidUTF8(value, "")))
	if utf8.RuneCountInString(value) <= maxCampaignValueRunes {
		return value
	}
	runes := []rune(value)
	return string(runes[:maxCampaignValueRunes])
}

// attributionFrom reads only explicit campaign labels or the referring host.
// It deliberately ignores IP addresses, user agents and visitor identifiers.
func attributionFrom(r *http.Request) campaignAttribution {
	q := r.URL.Query()
	a := campaignAttribution{
		Source:   strings.ToLower(cleanCampaignValue(q.Get("utm_source"))),
		Medium:   strings.ToLower(cleanCampaignValue(q.Get("utm_medium"))),
		Campaign: cleanCampaignValue(q.Get("utm_campaign")),
	}
	if a.Source == "" {
		a.Source = strings.ToLower(cleanCampaignValue(q.Get("ref")))
	}
	if a.Source != "" {
		return a
	}
	ref, err := url.Parse(r.Referer())
	if err == nil && ref.Hostname() != "" && !strings.EqualFold(ref.Hostname(), strings.Split(r.Host, ":")[0]) {
		a.Source = cleanCampaignValue(strings.ToLower(ref.Hostname()))
	}
	return a
}

func campaignQuery(r *http.Request) string {
	a := attributionFrom(r)
	q := make(url.Values)
	if a.Source != "" {
		if r.URL.Query().Get("utm_source") != "" {
			q.Set("utm_source", a.Source)
		} else {
			q.Set("ref", a.Source)
		}
	}
	if a.Medium != "" {
		q.Set("utm_medium", a.Medium)
	}
	if a.Campaign != "" {
		q.Set("utm_campaign", a.Campaign)
	}
	return q.Encode()
}

func withCampaign(path string, r *http.Request) string {
	if q := campaignQuery(r); q != "" {
		return path + "?" + q
	}
	return path
}

func likelyCrawler(r *http.Request) bool {
	ua := strings.ToLower(r.UserAgent())
	for _, marker := range []string{
		"bot", "crawler", "spider", "slurp", "facebookexternalhit",
		"linkedinbot", "twitterbot", "whatsapp", "discordbot",
	} {
		if strings.Contains(ua, marker) {
			return true
		}
	}
	return false
}

func (s *Server) recordMarketing(r *http.Request, kind string) {
	if likelyCrawler(r) {
		return
	}
	a := attributionFrom(r)
	if err := s.store.RecordMarketingEvent(r.Context(), store.MarketingEvent{
		Kind: kind, Source: a.Source, Medium: a.Medium, Campaign: a.Campaign,
		OccurredAt: time.Now().UTC(),
	}); err != nil {
		// Acquisition visibility must never make the acquisition path fail.
		s.log.Warn("marketing metric", "event", kind, "err", err)
	}
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	if s.currentUser(r) != nil {
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}
	s.recordMarketing(r, store.MarketingStart)
	http.Redirect(w, r, withCampaign("/signup", r), http.StatusSeeOther)
}
