//go:build unix

package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Otto's per-turn model routing. Otto defaults to a fast, cheap model for
// ordinary chat and escalates to a heavyweight model when the user's message
// is a coding task. A short Haiku classifier call (see execClassifier) decides
// which, so the expensive model is only paid for when code work actually
// warrants it.
const (
	ottoDefaultModel = "claude-haiku-4-5" // ordinary chat — the system prompt is small, so Haiku handles it well and cheaply
	ottoCodingModel  = "claude-opus-4-8"  // coding tasks — the router escalates here
	classifierModel  = "claude-haiku-4-5" // the router itself — cheap + fast
)

// classifyTimeout bounds the router subprocess so a slow or hung classify
// never stalls Otto's reply. On timeout the call fails and we fall back to
// the default (cheap) model.
const classifyTimeout = 20 * time.Second

// modelClassifier picks the model id for an Otto turn from the user's message.
// Implemented by execClassifier in production; faked in tests. A nil
// classifier on the handler means "don't route" — Otto inherits Claude Code's
// default model, which keeps existing tests and bare configs working.
type modelClassifier interface {
	classify(ctx context.Context, message string) string
}

// classifyPromptTmpl instructs the router to emit exactly one word. Kept
// deliberately tight: the cheaper and more constrained the prompt, the less
// the router itself costs and the less it can drift.
const classifyPromptTmpl = `You are a request router for a personal assistant named Otto. Decide whether the user's latest message needs CODE work or is ordinary CHAT.

CODE = writing, editing, debugging, reviewing, running, or explaining code; working in a git repo or codebase; build / test / deploy / lint tasks; creating or changing files in a software project.
CHAT = everything else: questions, planning, reminders, math, scheduling, life admin, casual conversation.

Reply with EXACTLY one word and nothing else: CODE or CHAT.

User message:
<<<
%s
>>>`

func classifyPrompt(message string) string {
	return fmt.Sprintf(classifyPromptTmpl, message)
}

// parseModelFromVerdict maps the router's raw text to a concrete model id.
// Only a clear CODE verdict (the first alphabetic token) escalates to the
// coding model; everything else — CHAT, empty output, or noise — maps to the
// default model. Defaulting to the cheap model on ambiguity is intentional:
// the failure mode of a misrouted turn should be "too cheap," not "too
// expensive."
func parseModelFromVerdict(raw string) string {
	letters := strings.FieldsFunc(strings.ToUpper(raw), func(r rune) bool {
		return r < 'A' || r > 'Z'
	})
	if len(letters) > 0 && letters[0] == "CODE" {
		return ottoCodingModel
	}
	return ottoDefaultModel
}

// modelLabel renders a model id for human-facing surfaces like /status.
func modelLabel(model string) string {
	switch model {
	case ottoCodingModel:
		return "opus-4.8 (coding)"
	case ottoDefaultModel:
		return "haiku-4.5 (chat)"
	case "":
		return "default (inherited)"
	default:
		return model
	}
}

// execClassifier runs the Haiku router as a short, self-contained `claude`
// subprocess. It deliberately does NOT reuse Otto's main runner: the router
// needs no MCP servers, no session (each call is a fresh one-shot), and no
// tools. --no-session-persistence keeps it from littering the disk with a new
// session per message, mirroring the proven prayer-checkin scripts.
type execClassifier struct {
	binary  string // path to the claude binary
	workDir string // cwd for the subprocess (Otto pins this to $HOME)
}

// classify returns the model Otto should use for this turn. Any failure
// (error, timeout, empty output) falls back to the default model and logs —
// routing must never block or break a reply.
func (c *execClassifier) classify(ctx context.Context, message string) string {
	message = strings.TrimSpace(message)
	if message == "" {
		// Nothing to classify (e.g. a photo-only message). Stay cheap.
		return ottoDefaultModel
	}
	cctx, cancel := context.WithTimeout(ctx, classifyTimeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, c.binary,
		"-p", classifyPrompt(message),
		"--model", classifierModel,
		"--no-session-persistence",
		"--dangerously-skip-permissions",
		"--disallowedTools", "*",
		"--output-format", "text",
	)
	if c.workDir != "" {
		cmd.Dir = c.workDir
	}
	cmd.Env = append(os.Environ(), "OTTO_RUNNING=1")

	out, err := cmd.Output()
	if err != nil {
		log.Printf("model router failed (%v) — defaulting to %s", err, ottoDefaultModel)
		return ottoDefaultModel
	}
	model := parseModelFromVerdict(string(out))
	log.Printf("model router: %q → %s", truncate(message, 60), modelLabel(model))
	return model
}
