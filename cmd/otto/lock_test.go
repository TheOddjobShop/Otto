//go:build unix

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestInstanceLockAcquireAndRelease(t *testing.T) {
	dir := t.TempDir()

	lock, err := acquireInstanceLock(dir)
	if err != nil {
		t.Fatalf("first acquire failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, lockFileName)); err != nil {
		t.Errorf("lock file was not created: %v", err)
	}
	lock.Release()

	// After release the lock must be immediately reusable — otherwise a
	// restart would be blocked by its own predecessor.
	again, err := acquireInstanceLock(dir)
	if err != nil {
		t.Fatalf("re-acquire after release failed: %v", err)
	}
	again.Release()
}

func TestInstanceLockRecordsPID(t *testing.T) {
	dir := t.TempDir()
	lock, err := acquireInstanceLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()

	data, err := os.ReadFile(filepath.Join(dir, lockFileName))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("lock file should contain a PID, got %q", data)
	}
	if pid != os.Getpid() {
		t.Errorf("lock file records pid %d, want %d", pid, os.Getpid())
	}
}

// An empty state dir disables locking rather than failing — used by tests and
// degenerate configs.
func TestInstanceLockNoDirIsNoOp(t *testing.T) {
	lock, err := acquireInstanceLock("")
	if err != nil {
		t.Fatalf("empty state dir should be a no-op, got %v", err)
	}
	lock.Release() // must not panic
}

func TestReleaseIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	lock, err := acquireInstanceLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	lock.Release()
	lock.Release() // must not panic or error
	var nilLock *instanceLock
	nilLock.Release()
}

// The behaviour that matters: a second process must be refused, with a message
// that says which process holds the lock and how to stop it.
//
// flock is per-process, so this needs a real second process — acquiring twice
// within one process would succeed and prove nothing.
func TestInstanceLockRefusesSecondProcess(t *testing.T) {
	if os.Getenv("OTTO_LOCK_CHILD") == "1" {
		// Child: try to take the lock its parent already holds.
		_, err := acquireInstanceLock(os.Getenv("OTTO_LOCK_DIR"))
		if err == nil {
			os.Exit(0) // acquired — the parent test will fail on this
		}
		os.Stderr.WriteString(err.Error())
		os.Exit(3)
	}

	dir := t.TempDir()
	lock, err := acquireInstanceLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()

	cmd := exec.Command(os.Args[0], "-test.run=TestInstanceLockRefusesSecondProcess")
	cmd.Env = append(os.Environ(), "OTTO_LOCK_CHILD=1", "OTTO_LOCK_DIR="+dir)
	out, err := cmd.CombinedOutput()

	var exitErr *exec.ExitError
	if err == nil {
		t.Fatal("a second process acquired the lock; two Otto instances would split Telegram updates between them")
	}
	if !asExitError(err, &exitErr) || exitErr.ExitCode() != 3 {
		t.Fatalf("child exited unexpectedly (%v): %s", err, out)
	}

	msg := string(out)
	for _, want := range []string{"already running", strconv.Itoa(os.Getpid()), "systemctl --user stop otto"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal message should mention %q:\n%s", want, msg)
		}
	}
}

func asExitError(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*target = e
	}
	return ok
}
