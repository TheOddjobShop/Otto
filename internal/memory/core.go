package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	mu      sync.RWMutex
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

// loadLocked reads both files without acquiring c.mu. Callers must already
// hold the lock (read or write).
func (c *Core) loadLocked() (user, memory string, err error) {
	if user, err = c.read(TargetUser); err != nil {
		return "", "", err
	}
	if memory, err = c.read(TargetMemory); err != nil {
		return "", "", err
	}
	return user, memory, nil
}

// Load returns (user, memory) file contents, "" for whichever is missing.
func (c *Core) Load() (user, memory string, err error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.loadLocked()
}

// Inject formats the core into a single block suitable for
// --append-system-prompt. Returns "" when both files are empty.
func (c *Core) Inject() (string, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	user, memory, err := c.loadLocked()
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
// Cap comparisons use rune count (Unicode characters) to match the documented
// "character ceiling" semantics; byte-counting would fire far too early for
// non-ASCII content such as CJK or emoji.
func (c *Core) Add(t Target, content string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
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
	if threshold := c.cap(t) * 80 / 100; len([]rune(next)) > threshold {
		return fmt.Errorf(
			"memory: at capacity (%d/%d chars over 80%% threshold) — consolidate existing entries with replace before adding. Current contents:\n%s",
			len([]rune(next)), c.cap(t), existing,
		)
	}
	return c.write(t, next)
}

// write persists body to the target file with 0600 perms, creating the
// directory if needed. Uses a uniquely-named temp file + rename for an atomic,
// collision-free swap.
func (c *Core) write(t Target, body string) error {
	if err := os.MkdirAll(c.dir, 0700); err != nil {
		return fmt.Errorf("memory: ensure dir: %w", err)
	}
	path := c.path(t)
	tmp, err := os.CreateTemp(c.dir, ".memwrite-*.tmp")
	if err != nil {
		return fmt.Errorf("memory: create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.WriteString(strings.TrimRight(body, "\n") + "\n"); err != nil {
		tmp.Close()
		return fmt.Errorf("memory: write %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("memory: sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("memory: close %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, 0600); err != nil {
		return fmt.Errorf("memory: chmod %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("memory: rename %s: %w", path, err)
	}
	if dir, derr := os.Open(c.dir); derr == nil {
		_ = dir.Sync()
		_ = dir.Close()
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
// content for unsafe material. A hard cap (100% of file cap) is enforced on
// the resulting body because Replace is given the full replacement text and
// there is no subsequent consolidation step that would catch an oversize write.
// Unlike Add, the 80% soft threshold is not used here — the caller is already
// performing a targeted edit, so the full cap is the appropriate ceiling.
// Matching is raw substring over the whole file; pass a distinctive snippet.
func (c *Core) Replace(t Target, oldText, content string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if strings.TrimSpace(oldText) == "" {
		return fmt.Errorf("memory: replace target must not be empty")
	}
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
	newBody := strings.Replace(body, oldText, content, 1)
	if len([]rune(newBody)) > c.cap(t) {
		return fmt.Errorf(
			"memory: replacement would exceed file cap (%d/%d chars) — shorten the new content",
			len([]rune(newBody)), c.cap(t),
		)
	}
	return c.write(t, newBody)
}

// Remove deletes the unique occurrence of oldText. Errors if absent or
// ambiguous. Leftover blank lines from the removed entry are collapsed.
// Matching is raw substring over the whole file; pass a distinctive snippet.
func (c *Core) Remove(t Target, oldText string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if strings.TrimSpace(oldText) == "" {
		return fmt.Errorf("memory: remove target must be non-empty")
	}
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
