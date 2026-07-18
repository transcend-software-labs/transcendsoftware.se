package builder

// Grok Build (xAI's coding-agent CLI) as an alternative build agent. Unlike
// opencode — a server the orchestrator drives over HTTP — grok runs HEADLESS:
// one `grok -p <instruction>` process launched inside the sandbox via the
// Machines exec API, streaming newline-delimited JSON events to a log file the
// runner polls. Everything around the agent (spawn, workspace restore, deploy
// verification, snapshot, screenshots, audit, polish round) is shared with the
// opencode path through the agentRunner seam in Build/finalize.
//
// Operational notes:
//   - The opencode server still boots in the sandbox (the spawn readiness
//     check pings it); it just idles. Zero changes to the opencode path.
//   - The CLI reads XAI_API_KEY + GROK_MODEL from the machine env (injected
//     per build, grok builds only) and picks up the workspace's AGENTS.md
//     natively.
//   - --always-approve is the headless equivalent of the auto-approve
//     permissions opencode needs in the sandbox (no human present).
//   - No session id exists for re-attach: an orchestrator restart reaps a
//     running grok build (snapshot preserved) instead of resuming it.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/fly"
	"github.com/transcend-software-labs/rasmus-ai/internal/opencode"
)

const (
	grokInstructionPath = "/tmp/grok-instruction.md"
	grokLogPath         = "/tmp/grok.ndjson"
	grokExitPath        = "/tmp/grok.exit"
	grokPollEvery       = 5 * time.Second
	grokPollChunk       = 200_000 // max log bytes fetched per poll
)

// grokRunner returns an agentRunner that executes the instruction with the
// Grok Build CLI inside the sandbox.
func (b *Sandbox) grokRunner(sb *fly.Sandbox, hooks Hooks) agentRunner {
	return func(ctx context.Context, instruction string) (opencode.Result, error) {
		return b.runGrok(ctx, sb.MachineID, instruction, hooks.OnLog)
	}
}

func (b *Sandbox) runGrok(ctx context.Context, machineID, instruction string, onLog func(string)) (opencode.Result, error) {
	// grok has no separate system-prompt input headless — the operating spec
	// becomes the instruction preamble (same content the opencode path sends as
	// its system prompt).
	full := instruction
	if b.cfg.SystemPrompt != "" {
		full = b.cfg.SystemPrompt + "\n\n---\n\n" + instruction
	}
	if err := b.writeGrokInstruction(ctx, machineID, full); err != nil {
		return opencode.Result{}, fmt.Errorf("grok: write instruction: %w", err)
	}

	// Launch detached: the CLI outlives individual exec calls; its NDJSON
	// stream lands in a file we poll, and its exit code in a marker file.
	// -m only when a model is pinned via GROK_MODEL — the CLI's own default is
	// the safe fallback (the model catalog is account-dependent; a wrong id
	// fails the whole run with "unknown model id").
	launch := `GROK_BIN=$(command -v grok || echo /root/.grok/bin/grok); ` +
		`MODELFLAG=""; [ -n "${GROK_MODEL:-}" ] && MODELFLAG="-m $GROK_MODEL"; ` +
		`rm -f ` + grokLogPath + ` ` + grokExitPath + `; ` +
		`cd /workspace && nohup sh -c '"$GROK_BIN" -p "$(cat ` + grokInstructionPath + `)" ` +
		`--always-approve --output-format streaming-json '"$MODELFLAG"' ` +
		`>` + grokLogPath + ` 2>&1; echo $? >` + grokExitPath + `' ` +
		`>/dev/null 2>&1 & echo launched`
	res, err := b.machines.Exec(ctx, machineID, []string{"/bin/sh", "-c", launch}, 30)
	if err != nil {
		return opencode.Result{}, fmt.Errorf("grok: launch: %w", err)
	}
	if res.ExitCode != 0 {
		return opencode.Result{}, fmt.Errorf("grok: launch failed: exit %d: %s", res.ExitCode, res.Stderr)
	}

	// Poll: stream new log bytes to onLog until the exit marker appears or the
	// build deadline cancels us.
	var log strings.Builder
	offset := 0
	ticker := time.NewTicker(grokPollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// The build window closed. Kill the agent so the snapshot save that
			// follows isn't starved by a still-churning process.
			kctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			_, _ = b.machines.Exec(kctx, machineID, []string{"/bin/sh", "-c", "pkill -9 -f 'grok ' 2>/dev/null; true"}, 20)
			cancel()
			return opencode.Result{Log: log.String()}, ctx.Err()
		case <-ticker.C:
		}

		chunk, done, exitCode, perr := b.pollGrok(ctx, machineID, offset)
		if perr != nil {
			// Transient exec hiccups happen (machine busy); keep polling — the
			// deadline bounds us.
			continue
		}
		if chunk != "" {
			offset += len(chunk)
			for _, line := range strings.Split(strings.TrimRight(chunk, "\n"), "\n") {
				if line == "" {
					continue
				}
				log.WriteString(line + "\n")
				if onLog != nil {
					if human := grokEventLine(line); human != "" {
						onLog(human)
					}
				}
			}
		}
		if done {
			if exitCode != 0 {
				tail := log.String()
				if len(tail) > 2000 {
					tail = tail[len(tail)-2000:]
				}
				return opencode.Result{Log: log.String()}, fmt.Errorf("grok exited with status %d: %s", exitCode, tail)
			}
			result := opencode.Result{Log: log.String()}
			result.Tokens, result.TokensInput = grokUsage(log.String())
			return result, nil
		}
	}
}

// writeGrokInstruction lands the (potentially large) instruction in the
// sandbox, base64-chunked so no shell-quoting or arg-length limit can mangle it.
func (b *Sandbox) writeGrokInstruction(ctx context.Context, machineID, instruction string) error {
	enc := base64.StdEncoding.EncodeToString([]byte(instruction))
	const chunk = 60_000
	for i := 0; i < len(enc); i += chunk {
		end := i + chunk
		if end > len(enc) {
			end = len(enc)
		}
		op := ">"
		if i > 0 {
			op = ">>"
		}
		cmd := fmt.Sprintf("printf '%%s' %s | base64 -d %s %s", enc[i:end], op, grokInstructionPath)
		res, err := b.machines.Exec(ctx, machineID, []string{"/bin/sh", "-c", cmd}, 30)
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("exit %d: %s", res.ExitCode, res.Stderr)
		}
	}
	return nil
}

// pollGrok fetches new log bytes from offset and reports completion.
func (b *Sandbox) pollGrok(ctx context.Context, machineID string, offset int) (chunk string, done bool, exitCode int, err error) {
	// One exec returns both: the delta (bounded) and, when present, the exit
	// marker on a sentinel-prefixed final line.
	cmd := fmt.Sprintf("tail -c +%d %s 2>/dev/null | head -c %d; test -f %s && printf '\\nGROKEXIT:%%s' \"$(cat %s)\"",
		offset+1, grokLogPath, grokPollChunk, grokExitPath, grokExitPath)
	res, execErr := b.machines.Exec(ctx, machineID, []string{"/bin/sh", "-c", cmd}, 60)
	if execErr != nil {
		return "", false, 0, execErr
	}
	out := res.Stdout
	if i := strings.LastIndex(out, "\nGROKEXIT:"); i >= 0 {
		code := strings.TrimSpace(out[i+len("\nGROKEXIT:"):])
		out = out[:i]
		n := 0
		_, _ = fmt.Sscanf(code, "%d", &n)
		// Only report done once the log tail has fully drained: if this chunk
		// was clipped at the poll limit, keep reading first.
		if len(out) < grokPollChunk {
			return out, true, n, nil
		}
	}
	return out, false, 0, nil
}

// grokEventLine turns one streaming-json event into a human log line, "" to
// skip. Unknown shapes fall back to the raw line so nothing is lost.
func grokEventLine(line string) string {
	var ev map[string]any
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		return line // not JSON — pass through verbatim
	}
	// Common fields across agent event streams, most-specific first.
	for _, k := range []string{"text", "message", "content", "summary"} {
		if s, ok := ev[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	if t, ok := ev["type"].(string); ok {
		if name, ok := ev["tool"].(string); ok {
			return t + ": " + name
		}
		switch t {
		case "ping", "heartbeat":
			return ""
		}
		return t
	}
	return ""
}

// grokUsage pulls token counts out of the event stream when a usage object is
// present (best effort — 0 when absent).
func grokUsage(log string) (total, input int) {
	for _, line := range strings.Split(log, "\n") {
		if !strings.Contains(line, "usage") {
			continue
		}
		var ev struct {
			Usage struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if ev.Usage.InputTokens+ev.Usage.OutputTokens > 0 {
			input = ev.Usage.InputTokens
			total = ev.Usage.InputTokens + ev.Usage.OutputTokens
		}
	}
	return total, input
}
