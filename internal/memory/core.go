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

// Add appends content as a new entry to the target file. It rejects unsafe
// content (see scanContent), exact-duplicate entries, and any write that would
// push the file past 80% of its cap — the over-capacity error includes the
// current contents so the caller (the model) can consolidate via Replace.
func (c *Core) Add(t Target, content string) error {
	content = strings.TrimSpace(content)
	if err := scanContent(content); err != nil {
		return err
	}
	existing, err := c.read(t)
	if err != nil {
		return err
	}
	if entryExists(existing, content) {
		return fmt.Errorf("memory: entry already present; not adding duplicate")
	}

	next := content
	if existing != "" {
		next = existing + "\n" + content
	}
	if threshold := c.cap(t) * 80 / 100; len(next) > threshold {
		return fmt.Errorf(
			"memory: at capacity (%d/%d chars over 80%% threshold) — consolidate existing entries with replace before adding. Current contents:\n%s",
			len(next), c.cap(t), existing,
		)
	}
	return c.write(t, next)
}

// write persists body to the target file with 0600 perms, creating the
// directory if needed. Uses tmp+rename for atomicity.
func (c *Core) write(t Target, body string) error {
	if err := os.MkdirAll(c.dir, 0700); err != nil {
		return fmt.Errorf("memory: ensure dir: %w", err)
	}
	path := c.path(t)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.TrimRight(body, "\n")+"\n"), 0600); err != nil {
		return fmt.Errorf("memory: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("memory: rename %s: %w", path, err)
	}
	return nil
}

// entryExists reports whether content appears as a complete line in body.
func entryExists(body, content string) bool {
	for _, line := range strings.Split(body, "\n") {
		if strings.TrimSpace(line) == content {
			return true
		}
	}
	return false
}

// Replace swaps the unique occurrence of oldText for content. It errors if
// oldText is absent or appears more than once (ambiguous), and scans the new
// content for unsafe material. Capacity is not re-checked because a replace
// that shrinks or holds size steady is always safe; a growing replace that
// breaches the cap is caught on the next Add.
func (c *Core) Replace(t Target, oldText, content string) error {
	content = strings.TrimSpace(content)
	if err := scanContent(content); err != nil {
		return err
	}
	body, err := c.read(t)
	if err != nil {
		return err
	}
	n := strings.Count(body, oldText)
	if n == 0 {
		return fmt.Errorf("memory: replace target %q not found", oldText)
	}
	if n > 1 {
		return fmt.Errorf("memory: replace target %q is ambiguous (%d matches)", oldText, n)
	}
	return c.write(t, strings.Replace(body, oldText, content, 1))
}

// Remove deletes the unique occurrence of oldText. Errors if absent or
// ambiguous. Leftover blank lines from the removed entry are collapsed.
func (c *Core) Remove(t Target, oldText string) error {
	body, err := c.read(t)
	if err != nil {
		return err
	}
	n := strings.Count(body, oldText)
	if n == 0 {
		return fmt.Errorf("memory: remove target %q not found", oldText)
	}
	if n > 1 {
		return fmt.Errorf("memory: remove target %q is ambiguous (%d matches)", oldText, n)
	}
	stripped := strings.Replace(body, oldText, "", 1)
	return c.write(t, collapseBlankLines(stripped))
}

// collapseBlankLines removes empty lines left behind by a removed entry.
func collapseBlankLines(s string) string {
	var keep []string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		keep = append(keep, line)
	}
	return strings.Join(keep, "\n")
}
