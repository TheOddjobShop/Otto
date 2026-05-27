package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Target selects which core file an operation applies to.
type Target int

const (
	TargetUser Target = iota
	TargetMemory
)

// Core manages the two curated-memory files in dir. Caps are measured in
// characters (a stable proxy for the token budget) and enforced on writes.
type Core struct {
	dir     string
	memCap  int // char cap for MEMORY.md
	userCap int // char cap for USER.md
}

// NewCore returns a Core rooted at dir. memCap/userCap are the per-file
// character ceilings (e.g. 2200 / 1375, roughly 800 / 500 tokens).
func NewCore(dir string, memCap, userCap int) *Core {
	return &Core{dir: dir, memCap: memCap, userCap: userCap}
}

func (c *Core) path(t Target) string {
	if t == TargetUser {
		return filepath.Join(c.dir, "USER.md")
	}
	return filepath.Join(c.dir, "MEMORY.md")
}

func (c *Core) cap(t Target) int {
	if t == TargetUser {
		return c.userCap
	}
	return c.memCap
}

// read returns the file's trimmed contents, or "" if it does not exist.
func (c *Core) read(t Target) (string, error) {
	body, err := os.ReadFile(c.path(t))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("memory: read %s: %w", c.path(t), err)
	}
	return strings.TrimRight(string(body), "\n"), nil
}

// Load returns (user, memory) file contents, "" for whichever is missing.
func (c *Core) Load() (user, memory string, err error) {
	if user, err = c.read(TargetUser); err != nil {
		return "", "", err
	}
	if memory, err = c.read(TargetMemory); err != nil {
		return "", "", err
	}
	return user, memory, nil
}

// Inject formats the core into a single block suitable for
// --append-system-prompt. Returns "" when both files are empty.
func (c *Core) Inject() (string, error) {
	user, memory, err := c.Load()
	if err != nil {
		return "", err
	}
	if user == "" && memory == "" {
		return "", nil
	}
	var b strings.Builder
	b.WriteString("───────────────────────────────────────────────\n")
	b.WriteString("  PERSISTENT MEMORY\n")
	b.WriteString("───────────────────────────────────────────────\n\n")
	if user != "" {
		b.WriteString("ABOUT THE USER:\n")
		b.WriteString(user)
		b.WriteString("\n\n")
	}
	if memory != "" {
		b.WriteString("WHAT YOU KNOW:\n")
		b.WriteString(memory)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n"), nil
}
