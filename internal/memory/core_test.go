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
