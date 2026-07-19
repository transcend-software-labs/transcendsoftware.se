package billing

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// VerifySignature validates a Stripe webhook against the raw request body and
// the endpoint's signing secret (the "Stripe-Signature" header). It enforces a
// timestamp tolerance to blunt replay, and accepts any of the header's v1
// signatures so secret rotation doesn't reject in-flight events. The signature
// is the sole authentication for the webhook route, so this must be strict.
func VerifySignature(payload []byte, header, secret string, now time.Time, tolerance time.Duration) error {
	if secret == "" {
		return fmt.Errorf("billing: no webhook secret configured")
	}
	var ts string
	var sigs []string
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			ts = v
		case "v1":
			sigs = append(sigs, v)
		}
	}
	if ts == "" || len(sigs) == 0 {
		return fmt.Errorf("billing: malformed signature header")
	}
	tsec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return fmt.Errorf("billing: bad signature timestamp")
	}
	if d := now.Sub(time.Unix(tsec, 0)); d < -tolerance || d > tolerance {
		return fmt.Errorf("billing: signature timestamp outside tolerance")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(payload)
	want := mac.Sum(nil)
	for _, s := range sigs {
		if got, err := hex.DecodeString(s); err == nil && hmac.Equal(got, want) {
			return nil
		}
	}
	return fmt.Errorf("billing: signature mismatch")
}

// Event is the minimal shape of a Stripe webhook event we act on.
type Event struct {
	Type   string
	Object EventObject
}

// EventObject carries just the fields the handled events need across
// checkout.session.* (a Session), customer.subscription.deleted (a
// Subscription) and invoice.payment_failed (an Invoice).
type EventObject struct {
	ID                string // the object's own id (a subscription id for subscription.* events)
	ClientReferenceID string // checkout session → the project id we set
	Customer          string // cus_...
	Subscription      string // checkout session → sub_...
	PaymentStatus     string // checkout session → "paid"
	Metadata          map[string]string
}

// ParseEvent extracts the event type and the object fields we use. It reads only
// what's needed — the full Stripe object is large and versioned.
func ParseEvent(payload []byte) (Event, error) {
	var raw struct {
		Type string `json:"type"`
		Data struct {
			Object struct {
				ID                string            `json:"id"`
				ClientReferenceID string            `json:"client_reference_id"`
				Customer          string            `json:"customer"`
				Subscription      string            `json:"subscription"`
				PaymentStatus     string            `json:"payment_status"`
				Metadata          map[string]string `json:"metadata"`
			} `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &raw); err != nil {
		return Event{}, fmt.Errorf("billing: decode event: %w", err)
	}
	o := raw.Data.Object
	return Event{Type: raw.Type, Object: EventObject{
		ID: o.ID, ClientReferenceID: o.ClientReferenceID, Customer: o.Customer,
		Subscription: o.Subscription, PaymentStatus: o.PaymentStatus, Metadata: o.Metadata,
	}}, nil
}

// ProjectID returns the project id an event maps to: the checkout session's
// client_reference_id, falling back to metadata[project_id] (which we set on
// both the session and the subscription).
func (e Event) ProjectID() string {
	if e.Object.ClientReferenceID != "" {
		return e.Object.ClientReferenceID
	}
	return e.Object.Metadata["project_id"]
}
