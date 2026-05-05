//go:build unix

package claude

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
)

// tailBuf is a bounded io.Writer that retains only the last cap bytes
// written to it. Used to capture the tail of claude's stdout for error
// diagnostics: stream-json output can be many MB, but if claude exits
// with a non-zero status the last few KB are usually where the failure
// surfaced.
type tailBuf struct {
	mu  sync.Mutex
	buf []byte
	cap int
}

func newTailBuf(capBytes int) *tailBuf { return &tailBuf{cap: capBytes} }

func (t *tailBuf) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.cap {
		t.buf = t.buf[len(t.buf)-t.cap:]
	}
	return len(p), nil
}

func (t *tailBuf) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// RunArgs is the per-call input to Runner.Run.
type RunArgs struct {
	Prompt     string
	SessionID  string
	ImagePaths []string // optional; appended to prompt as path references
	// AllowedTools is forwarded as --allowed-tools <csv>. Used by the
	// permission-button replay path: passing the just-approved tool
	// pattern here ensures the retry succeeds even if claude hasn't
	// re-read settings.json.
	AllowedTools []string
	// DisallowedTools is forwarded as --disallowedTools <csv>. Used by the
	// Toto fallback to deny everything ("*") so the lightweight assistant
	// can talk but can't act on the filesystem or call MCP servers.
	DisallowedTools []string
	// Model overrides Claude Code's default model selection (e.g.
	// "claude-haiku-4-5" for the Toto fallback). Empty = inherit default.
	Model string
	// Effort sets Claude Code's reasoning/effort level — one of "low",
	// "medium", "high", "xhigh", "max". Empty = inherit default. Used by
	// Toot so the announcement composer thinks before writing.
	Effort string
	// AppendSystemPrompt, when non-empty, replaces the runner's configured
	// systemPrompt for this single call. Used by Toto so a dynamic per-
	// call prompt (cat persona + Otto's in-flight prompt as context) can
	// be injected without rebuilding the runner.
	AppendSystemPrompt string
	Events             chan<- Event
}

// Runner runs Claude Code subprocesses.
type Runner interface {
	Run(ctx context.Context, args RunArgs) error
	WithEnv(extra map[string]string) Runner
}

type execRunner struct {
	binary        string
	mcpConfigPath string
	systemPrompt  string // appended to Claude Code's defaults; empty = no append
	workDir       string // cwd for the subprocess (Otto pins to $HOME)
	extraEnv      map[string]string
}

// NewExecRunner returns a Runner that invokes the given Claude Code binary.
// Otto inherits whatever auth Claude Code already has (from `claude /login`,
// `claude setup-token`, ANTHROPIC_API_KEY in the parent env, etc.) — Otto
// does not manage Anthropic credentials itself.
//
// systemPrompt, if non-empty, is passed via --append-system-prompt so it
// supplements (rather than replaces) Claude Code's built-in prompt.
//
// workDir is the working directory of each spawned `claude` subprocess.
// Otto pins this to the user's home so Claude has full filesystem reach.
func NewExecRunner(binary, mcpConfigPath, systemPrompt, workDir string) Runner {
	return &execRunner{
		binary:        binary,
		mcpConfigPath: mcpConfigPath,
		systemPrompt:  systemPrompt,
		workDir:       workDir,
	}
}

func (r *execRunner) WithEnv(extra map[string]string) Runner {
	merged := map[string]string{}
	for k, v := range r.extraEnv {
		merged[k] = v
	}
	for k, v := range extra {
		merged[k] = v
	}
	return &execRunner{
		binary:        r.binary,
		mcpConfigPath: r.mcpConfigPath,
		systemPrompt:  r.systemPrompt,
		workDir:       r.workDir,
		extraEnv:      merged,
	}
}

// buildCmdArgs constructs the argv passed to the Claude Code binary. Pulled
// out for unit testing — particularly to verify that --resume is only added
// when SessionID is non-empty (otherwise Claude Code rejects with "No
// conversation found"), and that --append-system-prompt is only added when
// a non-empty system prompt is configured.
//
// mcpConfigPath empty = no --mcp-config flag (used by the Toto fallback,
// which runs without any MCP servers).
func buildCmdArgs(prompt, sessionID, mcpConfigPath, systemPrompt, model, effort string, imagePaths, allowedTools, disallowedTools []string) []string {
	for _, p := range imagePaths {
		// Verify exact CLI syntax against the installed Claude Code version
		// during integration testing; this @path form is the documented
		// reference syntax at the time of writing.
		prompt += " @" + p
	}
	cmdArgs := []string{
		"-p", prompt,
		"--output-format", "stream-json",
		"--verbose",
		// Skip the interactive tool-permission prompt: in -p mode there's
		// no human-in-the-loop to approve, and Otto's threat model is a
		// single-user allowlisted bot on the user's own home server. The
		// upstream Telegram sender is already gated by internal/auth, so
		// every message that reaches claude is by definition the owner.
		"--dangerously-skip-permissions",
	}
	if mcpConfigPath != "" {
		cmdArgs = append(cmdArgs, "--mcp-config", mcpConfigPath)
	}
	if model != "" {
		cmdArgs = append(cmdArgs, "--model", model)
	}
	if effort != "" {
		cmdArgs = append(cmdArgs, "--effort", effort)
	}
	if systemPrompt != "" {
		cmdArgs = append(cmdArgs, "--append-system-prompt", systemPrompt)
	}
	if len(allowedTools) > 0 {
		cmdArgs = append(cmdArgs, "--allowed-tools", strings.Join(allowedTools, ","))
	}
	if len(disallowedTools) > 0 {
		cmdArgs = append(cmdArgs, "--disallowedTools", strings.Join(disallowedTools, ","))
	}
	// Only --resume an existing session. Empty SessionID means start fresh —
	// Claude Code will allocate a new session ID, which our parser captures
	// from the system/init event and the caller persists.
	if sessionID != "" {
		cmdArgs = append(cmdArgs, "--resume", sessionID)
	}
	return cmdArgs
}

func (r *execRunner) Run(ctx context.Context, args RunArgs) error {
	systemPrompt := r.systemPrompt
	if args.AppendSystemPrompt != "" {
		systemPrompt = args.AppendSystemPrompt
	}
	cmdArgs := buildCmdArgs(args.Prompt, args.SessionID, r.mcpConfigPath, systemPrompt, args.Model, args.Effort, args.ImagePaths, args.AllowedTools, args.DisallowedTools)
	cmd := exec.CommandContext(ctx, r.binary, cmdArgs...)
	if r.workDir != "" {
		cmd.Dir = r.workDir
	}
	cmd.Env = os.Environ()
	for k, v := range r.extraEnv {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	// Put process in own group so we can kill children on timeout.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	// Tee stdout into a bounded tail buffer so on a non-zero exit we have
	// some idea of what claude was emitting before it died. ParseStream
	// reads from the teed reader; bytes still flow through to it.
	stdoutTail := newTailBuf(64 * 1024)
	teedStdout := io.TeeReader(stdout, stdoutTail)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("claude: start: %w", err)
	}

	parseDone := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		parseDone <- ParseStream(ctx, teedStdout, args.Events)
	}()

	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	select {
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-waitDone
		wg.Wait()
		_ = drain(stdout)
		return ctx.Err()
	case waitErr := <-waitDone:
		wg.Wait()
		parseErr := <-parseDone
		if waitErr != nil {
			stderrText := strings.TrimSpace(stderr.String())
			tailText := strings.TrimSpace(stdoutTail.String())
			// Always log the full picture to the journal — Telegram replies
			// are necessarily terse, but the daemon log is where we want
			// to be able to dig in after a failure.
			log.Printf("claude failed: %v\n--- stderr ---\n%s\n--- stdout (last %d bytes) ---\n%s\n--- parse-err ---\n%v\n--- end ---",
				waitErr, stderrText, len(tailText), tailText, parseErr)
			return fmt.Errorf("claude: %w: %s", waitErr, buildErrorInfo(stderrText, tailText, parseErr))
		}
		if parseErr != nil {
			return parseErr
		}
		return nil
	}
}

func drain(r io.Reader) error {
	_, err := io.Copy(io.Discard, r)
	return err
}

// buildErrorInfo composes the human-readable suffix appended to the claude
// error message. Prefer stderr (claude's intended diagnostic channel); fall
// back to the tail of stdout (where stream-json failures sometimes surface);
// finally a parser error. Truncates so the Telegram message stays usable.
func buildErrorInfo(stderrText, stdoutTail string, parseErr error) string {
	const maxLen = 1500
	pick := stderrText
	if pick == "" {
		pick = stdoutTail
	}
	if pick == "" && parseErr != nil {
		pick = "parser: " + parseErr.Error()
	}
	if pick == "" {
		return "(no output; check `journalctl --user -u otto`)"
	}
	if len(pick) > maxLen {
		pick = "...\n" + pick[len(pick)-maxLen:]
	}
	return pick
}
