package web

import (
	"testing"
	"time"
)

func TestAttemptLimiterExpiresAndSweepsWindows(t *testing.T) {
	l := newAttemptLimiter()
	now := time.Now().UTC()
	if !l.allow("login:ip:one", 1, time.Minute, now) {
		t.Fatal("first attempt was rejected")
	}
	if l.allow("login:ip:one", 1, time.Minute, now.Add(time.Second)) {
		t.Fatal("attempt over the limit was allowed")
	}
	if !l.allow("login:ip:two", 1, time.Minute, now.Add(2*time.Minute)) {
		t.Fatal("new window was rejected")
	}
	if _, ok := l.windows["login:ip:one"]; ok {
		t.Fatal("expired attacker-controlled key was not swept")
	}
}
