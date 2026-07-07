package store

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestValidUTF8_ScrubsBinaryBytes(t *testing.T) {
	// 0xc3 0xe2 is the exact invalid sequence that failed a real build (a
	// truncated multibyte char / binary bytes from the agent reading a JPEG).
	in := "log line \xc3\xe2 with bad bytes and åäö ok"
	if utf8.ValidString(in) {
		t.Fatal("test input should be invalid UTF-8")
	}
	got := validUTF8(in)
	if !utf8.ValidString(got) {
		t.Fatalf("result still invalid UTF-8: %q", got)
	}
	for _, want := range []string{"log line", "with bad bytes", "åäö ok"} {
		if !strings.Contains(got, want) {
			t.Errorf("scrub dropped valid content %q: %q", want, got)
		}
	}
}
