//go:build unix

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Single-instance locking.
//
// Two Otto processes long-polling Telegram with one bot token do not error.
// Telegram hands each update to whichever poller asks for it first, so messages
// land in one process or the other essentially at random. The symptom is "Otto
// sometimes ignores me", and it is genuinely miserable to diagnose: the bot is
// running, the logs look healthy, and roughly half the messages are simply
// answered by a process you are not watching.
//
// Before `otto tui` existed there was only one way to launch Otto, so the
// hazard was unreachable. Now there are two, and the obvious thing to do —
// leaving the service running and starting the TUI — is exactly the thing that
// triggers it. Hence a lock rather than a note in the README.

// lockFileName is the advisory lock, kept in the state directory beside
// state.db so it shares the directory's lifetime and permissions.
const lockFileName = "otto.lock"

// instanceLock is an held flock. Close releases it.
type instanceLock struct {
	f *os.File
}

// acquireInstanceLock takes an exclusive, non-blocking lock for stateDir.
//
// On contention it returns an error naming the PID holding the lock and the
// command to stop it, because "another instance is running" without saying
// which leaves the user to go hunting.
//
// The lock is advisory and process-scoped: the kernel drops it when the holder
// exits for any reason, including a crash or SIGKILL, so a stale lock file can
// never wedge a restart. That is why flock is used rather than a PID file,
// which has to be validated and cleaned up and gets both wrong eventually.
func acquireInstanceLock(stateDir string) (*instanceLock, error) {
	if stateDir == "" {
		// Nothing to lock against — used by tests and by degenerate configs.
		return &instanceLock{}, nil
	}
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return nil, fmt.Errorf("lock: create state dir: %w", err)
	}
	path := filepath.Join(stateDir, lockFileName)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("lock: open %s: %w", path, err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		holder := readLockHolder(f)
		f.Close()
		return nil, fmt.Errorf("another Otto is already running%s.\n\n"+
			"Only one process may poll Telegram with a given bot token — two would split\n"+
			"your messages between them at random. Stop the other one first:\n\n"+
			"  systemctl --user stop otto        # Linux\n"+
			"  launchctl bootout gui/$(id -u)/com.otto.bot   # macOS\n\n"+
			"then try again.", holder)
	}

	// Record the PID for the benefit of whoever collides with us next. Errors
	// are non-fatal: the lock itself is held regardless, and the PID is only
	// there to make the eventual error message useful.
	if err := f.Truncate(0); err == nil {
		if _, err := f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0); err != nil {
			_ = err // best effort
		}
	}
	return &instanceLock{f: f}, nil
}

// readLockHolder returns a " (pid N)" suffix when the lock file names a PID.
func readLockHolder(f *os.File) string {
	buf := make([]byte, 32)
	n, _ := f.ReadAt(buf, 0)
	if n <= 0 {
		return ""
	}
	pid := strings.TrimSpace(string(buf[:n]))
	if pid == "" {
		return ""
	}
	if _, err := strconv.Atoi(pid); err != nil {
		return ""
	}
	return " (pid " + pid + ")"
}

// Release drops the lock. Safe on a zero-value lock.
func (l *instanceLock) Release() {
	if l == nil || l.f == nil {
		return
	}
	// Unlock explicitly rather than relying on close: on some systems the two
	// are equivalent, but being explicit means the intent survives a future
	// refactor that keeps the descriptor open for something else.
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
	l.f = nil
}
