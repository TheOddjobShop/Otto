package memory

import (
	"strings"
	"testing"
)

func newTestCore(t *testing.T) *Core {
	t.Helper()
	return NewCore(t.TempDir(), 2200, 1375) // memCap, userCap (chars)
}

func TestLoadMissingFilesIsEmpty(t *testing.T) {
	c := newTestCore(t)
	user, mem, err := c.Load()
	if err != nil {
		t.Fatalf("Load on empty dir: %v", err)
	}
	if user != "" || mem != "" {
		t.Fatalf("expected empty strings, got user=%q mem=%q", user, mem)
	}
}

func TestInjectEmptyIsEmpty(t *testing.T) {
	c := newTestCore(t)
	got, err := c.Inject()
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if strings.TrimSpace(got) != "" {
		t.Fatalf("empty core should inject empty, got %q", got)
	}
}

func TestInjectFormatsBothSections(t *testing.T) {
	c := newTestCore(t)
	if err := c.Add(TargetUser, "User is named Justin."); err != nil {
		t.Fatalf("Add user: %v", err)
	}
	if err := c.Add(TargetMemory, "Server runs Arch Linux."); err != nil {
		t.Fatalf("Add memory: %v", err)
	}
	got, err := c.Inject()
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if !strings.Contains(got, "Justin") || !strings.Contains(got, "Arch Linux") {
		t.Fatalf("inject missing content: %q", got)
	}
	if strings.Index(got, "Justin") > strings.Index(got, "Arch Linux") {
		t.Fatalf("expected USER section before MEMORY section: %q", got)
	}
}

func TestAddAppendsEntries(t *testing.T) {
	c := newTestCore(t)
	if err := c.Add(TargetMemory, "fact one"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := c.Add(TargetMemory, "fact two"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	_, mem, _ := c.Load()
	if !strings.Contains(mem, "fact one") || !strings.Contains(mem, "fact two") {
		t.Fatalf("both facts should be present: %q", mem)
	}
}

func TestAddRejectsExactDuplicate(t *testing.T) {
	c := newTestCore(t)
	if err := c.Add(TargetMemory, "duplicate fact"); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	if err := c.Add(TargetMemory, "duplicate fact"); err == nil {
		t.Fatal("exact duplicate entry should be rejected")
	}
}

func TestAddRejectsUnsafe(t *testing.T) {
	c := newTestCore(t)
	if err := c.Add(TargetMemory, "key sk-ant-api03-doNotStoreThisSecret"); err == nil {
		t.Fatal("secret content should be rejected by Add")
	}
}

func TestAddErrorsAtCapacity(t *testing.T) {
	c := NewCore(t.TempDir(), 2200, 100)
	big := strings.Repeat("x", 85)
	err := c.Add(TargetUser, big)
	if err == nil {
		t.Fatal("Add over 80% capacity should error")
	}
	if !strings.Contains(err.Error(), "capacity") {
		t.Fatalf("capacity error should mention capacity, got: %v", err)
	}
	if !strings.Contains(err.Error(), "consolidate") {
		t.Fatalf("capacity error should prompt consolidation, got: %v", err)
	}
}

func TestReplaceSubstring(t *testing.T) {
	c := newTestCore(t)
	if err := c.Add(TargetUser, "User prefers dark mode everywhere."); err != nil {
		t.Fatalf("Add: %v", err)
	}
	err := c.Replace(TargetUser, "dark mode everywhere", "light mode in editor, dark in terminal")
	if err != nil {
		t.Fatalf("Replace: %v", err)
	}
	user, _, _ := c.Load()
	if !strings.Contains(user, "light mode in editor") {
		t.Fatalf("replacement not applied: %q", user)
	}
	if strings.Contains(user, "dark mode everywhere") {
		t.Fatalf("old text still present: %q", user)
	}
}

func TestReplaceMissingErrors(t *testing.T) {
	c := newTestCore(t)
	if err := c.Add(TargetUser, "some fact"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := c.Replace(TargetUser, "nonexistent", "x"); err == nil {
		t.Fatal("Replace of missing text should error")
	}
}

func TestReplaceAmbiguousErrors(t *testing.T) {
	c := newTestCore(t)
	if err := c.Add(TargetMemory, "foo and foo again"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := c.Replace(TargetMemory, "foo", "bar"); err == nil {
		t.Fatal("ambiguous (multi-match) Replace should error")
	}
}

func TestReplaceScansNewContent(t *testing.T) {
	c := newTestCore(t)
	if err := c.Add(TargetUser, "harmless fact"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := c.Replace(TargetUser, "harmless fact", "sk-ant-api03-secretLeak"); err == nil {
		t.Fatal("Replace must scan the new content")
	}
}

func TestRemoveSubstring(t *testing.T) {
	c := newTestCore(t)
	if err := c.Add(TargetMemory, "keep this"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := c.Add(TargetMemory, "remove this"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := c.Remove(TargetMemory, "remove this"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	_, mem, _ := c.Load()
	if strings.Contains(mem, "remove this") {
		t.Fatalf("entry not removed: %q", mem)
	}
	if !strings.Contains(mem, "keep this") {
		t.Fatalf("wrong entry removed: %q", mem)
	}
}

func TestRemoveMissingErrors(t *testing.T) {
	c := newTestCore(t)
	if err := c.Remove(TargetMemory, "not there"); err == nil {
		t.Fatal("Remove of missing text should error")
	}
}
