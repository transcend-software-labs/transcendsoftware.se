package builder

import (
	"context"
	"encoding/base64"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/fly"
	"github.com/transcend-software-labs/rasmus-ai/internal/opencode"
)

// grokMachines scripts the Machines exec surface the grok runner drives: it
// decodes the instruction writes, accepts the launch, and serves a canned
// NDJSON log + exit marker across polls.
type grokMachines struct {
	*fly.Fake
	mu          sync.Mutex
	cmds        []string // every exec'd command, for shape assertions
	instruction strings.Builder
	launched    bool
	log         string // full canned NDJSON the "CLI" produces
	exit        string // exit-code file content, served once launched
	polls       int
}

func (m *grokMachines) Exec(_ context.Context, machineID string, command []string, _ int) (fly.ExecResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cmd := strings.Join(command, " ")
	m.cmds = append(m.cmds, cmd)
	switch {
	case strings.Contains(cmd, "base64 -d"):
		// Instruction chunk: printf '%s' <b64> | base64 -d >(>) path
		fields := strings.Fields(cmd)
		for i, f := range fields {
			if f == "'%s'" && i+1 < len(fields) {
				raw, err := base64.StdEncoding.DecodeString(fields[i+1])
				if err != nil {
					return fly.ExecResult{ExitCode: 1, Stderr: "bad b64"}, nil
				}
				m.instruction.Write(raw)
			}
		}
		return fly.ExecResult{ExitCode: 0}, nil
	case strings.Contains(cmd, "nohup"):
		m.launched = true
		return fly.ExecResult{ExitCode: 0, Stdout: "launched"}, nil
	case strings.Contains(cmd, "GROKEXIT"):
		if !m.launched {
			return fly.ExecResult{ExitCode: 0}, nil
		}
		m.polls++
		if m.polls == 1 {
			// First poll: half the log, not done yet.
			half := m.log[:len(m.log)/2]
			return fly.ExecResult{ExitCode: 0, Stdout: half}, nil
		}
		// Later polls: the rest + the exit marker.
		rest := m.log[len(m.log)/2:]
		return fly.ExecResult{ExitCode: 0, Stdout: rest + "\nGROKEXIT:" + m.exit}, nil
	}
	return fly.ExecResult{ExitCode: 0}, nil
}

func newGrokSandbox(m *grokMachines) *Sandbox {
	return NewSandbox(m, func(string) opencode.Driver { return opencode.NewFake() },
		Config{SystemPrompt: "OPERATING SPEC", GrokAPIKey: "xk"})
}

func TestRunGrok_StreamsAndSucceeds(t *testing.T) {
	m := &grokMachines{Fake: fly.NewFake(),
		log: `{"type":"start"}` + "\n" +
			`{"type":"message","text":"Building the site"}` + "\n" +
			`{"type":"tool_use","tool":"bash"}` + "\n" +
			`{"usage":{"input_tokens":1000,"output_tokens":250}}` + "\n" +
			`{"type":"done"}`,
		exit: "0"}
	b := newGrokSandbox(m)

	var lines []string
	res, err := b.runGrokFast(t, m, "Build the plan", func(s string) { lines = append(lines, s) })
	if err != nil {
		t.Fatalf("runGrok: %v", err)
	}
	// The instruction carries the operating spec preamble + the plan.
	got := m.instruction.String()
	if !strings.HasPrefix(got, "OPERATING SPEC") || !strings.Contains(got, "Build the plan") {
		t.Fatalf("instruction wrong: %q", got)
	}
	if !m.launched {
		t.Fatal("grok was never launched")
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "Building the site") {
		t.Fatalf("streamed lines missing agent text: %v", lines)
	}
	if res.Tokens != 1250 || res.TokensInput != 1000 {
		t.Fatalf("usage not extracted: total=%d input=%d", res.Tokens, res.TokensInput)
	}
	if !strings.Contains(res.Log, `"type":"done"`) {
		t.Fatal("full NDJSON log not accumulated")
	}
}

func TestRunGrok_NonzeroExitFails(t *testing.T) {
	m := &grokMachines{Fake: fly.NewFake(), log: `{"type":"error","message":"boom"}`, exit: "1"}
	b := newGrokSandbox(m)
	_, err := b.runGrokFast(t, m, "x", nil)
	if err == nil || !strings.Contains(err.Error(), "status 1") {
		t.Fatalf("nonzero exit must fail with the status, got %v", err)
	}
}

// runGrokFast runs runGrok with a short poll interval by scaling time via a
// tight context; the production ticker is 5s, fine for one or two polls.
func (b *Sandbox) runGrokFast(t *testing.T, m *grokMachines, instruction string, onLog func(string)) (opencode.Result, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return b.runGrok(ctx, "m1", instruction, onLog)
}

// Guard: the launch command must reference the CLI defensively (PATH or the
// installer's home) and must not hardcode a model when GROK_MODEL is unset.
func TestGrokLaunchCommandShape(t *testing.T) {
	m := &grokMachines{Fake: fly.NewFake(), log: "{}", exit: "0"}
	b := newGrokSandbox(m)
	_, _ = b.runGrokFast(t, m, "x", nil)

	m.mu.Lock()
	var launch string
	for _, c := range m.cmds {
		if strings.Contains(c, "nohup") {
			launch = c
		}
	}
	m.mu.Unlock()
	if launch == "" {
		t.Fatal("launch command never issued")
	}
	for _, want := range []string{"--always-approve", "--output-format streaming-json", "/root/.grok/bin/grok", "GROK_MODEL"} {
		if !strings.Contains(launch, want) {
			t.Errorf("launch command missing %q:\n%s", want, launch)
		}
	}
	if strings.Contains(launch, "-m grok-4.5") {
		t.Error("launch must not hardcode a model id")
	}
}
