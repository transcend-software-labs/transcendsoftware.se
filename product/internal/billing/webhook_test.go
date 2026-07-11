package billing

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
	"time"
)

// sign builds a valid Stripe-Signature header for payload at time t.
func sign(secret, payload string, t time.Time) string {
	ts := fmt.Sprintf("%d", t.Unix())
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "." + payload))
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifySignature(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	secret, body := "whsec_test", `{"type":"checkout.session.completed"}`
	tol := 5 * time.Minute

	if err := VerifySignature([]byte(body), sign(secret, body, now), secret, now, tol); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	// Tampered body.
	if err := VerifySignature([]byte(body+" "), sign(secret, body, now), secret, now, tol); err == nil {
		t.Error("tampered body accepted")
	}
	// Wrong secret.
	if err := VerifySignature([]byte(body), sign("whsec_other", body, now), secret, now, tol); err == nil {
		t.Error("wrong-secret signature accepted")
	}
	// Stale timestamp (6 min old).
	if err := VerifySignature([]byte(body), sign(secret, body, now.Add(-6*time.Minute)), secret, now, tol); err == nil {
		t.Error("stale timestamp accepted")
	}
	// Rotation: a header with a bad v1 AND the good v1 must pass.
	good := sign(secret, body, now)
	rotated := good + ",v1=deadbeef"
	if err := VerifySignature([]byte(body), rotated, secret, now, tol); err != nil {
		t.Errorf("rotation (multi-v1) rejected: %v", err)
	}
	// Garbage header, and empty secret.
	if err := VerifySignature([]byte(body), "not-a-signature", secret, now, tol); err == nil {
		t.Error("garbage header accepted")
	}
	if err := VerifySignature([]byte(body), sign(secret, body, now), "", now, tol); err == nil {
		t.Error("empty secret accepted")
	}
}

func TestParseEvent(t *testing.T) {
	checkout := `{"type":"checkout.session.completed","data":{"object":{
		"id":"cs_1","client_reference_id":"proj-9","customer":"cus_1","subscription":"sub_1",
		"payment_status":"paid","metadata":{"project_id":"proj-9"}}}}`
	e, err := ParseEvent([]byte(checkout))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if e.Type != "checkout.session.completed" || e.Object.Customer != "cus_1" ||
		e.Object.Subscription != "sub_1" || e.ProjectID() != "proj-9" {
		t.Fatalf("checkout parse wrong: %+v", e)
	}

	// A subscription.deleted event: the object IS the subscription; project id
	// rides on metadata, its own id is the sub id.
	del := `{"type":"customer.subscription.deleted","data":{"object":{
		"id":"sub_1","customer":"cus_1","metadata":{"project_id":"proj-9"}}}}`
	e2, _ := ParseEvent([]byte(del))
	if e2.ProjectID() != "proj-9" || e2.Object.ID != "sub_1" {
		t.Fatalf("subscription.deleted parse wrong: %+v", e2)
	}

	if _, err := ParseEvent([]byte("{not json")); err == nil {
		t.Error("expected error on bad JSON")
	}
}
