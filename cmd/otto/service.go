//go:build unix

package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"time"
)

// Service handover.
//
// The instance lock means the background service and `otto tui` cannot both
// run. That is correct — two pollers sharing one bot token would split messages
// between them at random — but it makes the obvious action fail:
//
//	$ otto tui
//	another Otto is already running (pid 1234).
//
// Telling the user to go stop a service and remember to start it again puts the
// burden of a correctness constraint onto them, every single time. So the TUI
// does it: stop the service, run, start it back on exit. The lock still exists
// and still refuses anything it cannot account for — this only automates the
// one case where the holder is a service manager the user owns.

// serviceCtl controls the platform's user-level service manager.
type serviceCtl struct {
	// Name is what to call it when talking to the user.
	Name string
	// stop and start run the manager. Separated so tests can substitute them.
	stop  func() error
	start func() error
	// active reports whether the service is currently running.
	active func() bool
}

// findService is the service-detection hook. A package var so tests can
// substitute a fake manager without a real systemd or launchd.
var findService = detectService

// detectService returns the service manager controlling Otto on this platform,
// or nil when Otto is not installed as a service (a manual run, a container, a
// machine where setup.sh never got that far).
func detectService() *serviceCtl {
	switch runtime.GOOS {
	case "linux":
		if _, err := exec.LookPath("systemctl"); err != nil {
			return nil
		}
		// A unit that was never installed is not something to manage.
		if err := exec.Command("systemctl", "--user", "cat", "otto.service").Run(); err != nil {
			return nil
		}
		return &serviceCtl{
			Name:  "otto.service",
			stop:  func() error { return exec.Command("systemctl", "--user", "stop", "otto.service").Run() },
			start: func() error { return exec.Command("systemctl", "--user", "start", "otto.service").Run() },
			active: func() bool {
				return exec.Command("systemctl", "--user", "is-active", "--quiet", "otto.service").Run() == nil
			},
		}
	case "darwin":
		if _, err := exec.LookPath("launchctl"); err != nil {
			return nil
		}
		target := "gui/" + strconv.Itoa(os.Getuid()) + "/com.otto.bot"
		if err := exec.Command("launchctl", "print", target).Run(); err != nil {
			return nil
		}
		return &serviceCtl{
			Name: "com.otto.bot",
			stop: func() error { return exec.Command("launchctl", "bootout", target).Run() },
			start: func() error {
				return exec.Command("launchctl", "bootstrap", "gui/"+strconv.Itoa(os.Getuid()), launchdPlistPath()).Run()
			},
			active: func() bool {
				return exec.Command("launchctl", "print", target).Run() == nil
			},
		}
	}
	return nil
}

func launchdPlistPath() string {
	home, _ := os.UserHomeDir()
	return home + "/Library/LaunchAgents/com.otto.bot.plist"
}

// lockWaitTimeout bounds how long we wait for a stopped service to release the
// lock. Shutdown drains in-flight dispatches, so a turn already running can add
// a few seconds; beyond this something is wrong and the user should see the
// ordinary refusal rather than a hang.
// Package var rather than const so tests can shorten it.
var lockWaitTimeout = 20 * time.Second

// acquireLockWithHandover takes the instance lock, stopping the service first
// when it is what holds it.
//
// Returns the lock and a release func that restores the previous state — the
// caller defers it, so the service comes back whether the TUI exits cleanly,
// on ctrl+c, or by error. If no service was stopped the release func just drops
// the lock.
func acquireLockWithHandover(stateDir string, allowHandover bool) (*instanceLock, func(), error) {
	lock, err := acquireInstanceLock(stateDir)
	if err == nil {
		return lock, func() { lock.Release() }, nil
	}

	svc := findService()
	if !allowHandover || svc == nil || !svc.active() {
		// Nothing we can account for holds the lock — another terminal, a
		// stale process, something hand-started. Refusing is right; guessing
		// and killing it would not be.
		return nil, nil, err
	}

	fmt.Fprintf(os.Stderr, "otto: %s is running; stopping it for this session…\n", svc.Name)
	if stopErr := svc.stop(); stopErr != nil {
		return nil, nil, fmt.Errorf("could not stop %s: %w\n\noriginal error: %v", svc.Name, stopErr, err)
	}

	// Wait for the lock rather than assuming stop is synchronous: systemd
	// returns once it has *sent* SIGTERM, and Otto's shutdown drains in-flight
	// dispatches before releasing.
	deadline := time.Now().Add(lockWaitTimeout)
	for {
		lock, err = acquireInstanceLock(stateDir)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			// Put it back — leaving the user's bot stopped because we failed
			// to start would be a strictly worse outcome than not starting.
			_ = svc.start()
			return nil, nil, fmt.Errorf("%s stopped but the lock was still held after %s; restarted it and gave up", svc.Name, lockWaitTimeout)
		}
		time.Sleep(250 * time.Millisecond)
	}

	fmt.Fprintf(os.Stderr, "otto: took over from %s — it restarts when you quit.\n", svc.Name)
	return lock, func() {
		lock.Release()
		fmt.Fprintf(os.Stderr, "otto: restarting %s…\n", svc.Name)
		if startErr := svc.start(); startErr != nil {
			// Loud, because the user's bot is now down and they would
			// otherwise only find out when it stops answering their phone.
			fmt.Fprintf(os.Stderr,
				"otto: WARNING — could not restart %s: %v\n"+
					"      Your bot is not running. Start it with:\n"+
					"        %s\n", svc.Name, startErr, manualStartHint())
		}
	}, nil
}

func manualStartHint() string {
	if runtime.GOOS == "darwin" {
		return "launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.otto.bot.plist"
	}
	return "systemctl --user start otto"
}
