//go:build unix

package main

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// fakeService records what the handover asked of the service manager.
type fakeService struct {
	running  bool
	stops    int
	starts   int
	stopErr  error
	startErr error
}

func (f *fakeService) ctl() *serviceCtl {
	return &serviceCtl{
		Name: "otto.service",
		stop: func() error {
			if f.stopErr != nil {
				return f.stopErr
			}
			f.stops++
			f.running = false
			return nil
		},
		start: func() error {
			if f.startErr != nil {
				return f.startErr
			}
			f.starts++
			f.running = true
			return nil
		},
		active: func() bool { return f.running },
	}
}

// withService swaps the detection hook for the duration of a test.
func withService(t *testing.T, svc *serviceCtl) {
	t.Helper()
	prev := findService
	findService = func() *serviceCtl { return svc }
	t.Cleanup(func() { findService = prev })
}

// The ordinary case: nothing else holds the lock, so no service is touched.
func TestHandoverNotNeededWhenLockIsFree(t *testing.T) {
	f := &fakeService{running: true}
	withService(t, f.ctl())

	lock, release, err := acquireLockWithHandover(t.TempDir(), true)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer release()

	if lock == nil {
		t.Error("expected a held lock")
	}
	if f.stops != 0 {
		t.Errorf("stopped the service %d times; nothing was holding the lock", f.stops)
	}
}

// The case the whole feature exists for: the service holds the lock, so stop
// it, run, and put it back on exit.
func TestHandoverStopsAndRestartsService(t *testing.T) {
	dir := t.TempDir()

	// Stand in for the running service by holding the lock ourselves, and
	// release it when "stopped".
	held, err := acquireInstanceLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeService{running: true}
	svc := f.ctl()
	realStop := svc.stop
	svc.stop = func() error {
		if err := realStop(); err != nil {
			return err
		}
		held.Release() // the service exiting drops its lock
		return nil
	}
	withService(t, svc)

	lock, release, err := acquireLockWithHandover(dir, true)
	if err != nil {
		t.Fatalf("handover failed: %v", err)
	}
	if lock == nil {
		t.Fatal("expected a held lock after handover")
	}
	if f.stops != 1 {
		t.Errorf("stopped %d times, want exactly 1", f.stops)
	}
	if f.starts != 0 {
		t.Error("the service should not be restarted until the TUI exits")
	}

	release()
	if f.starts != 1 {
		t.Errorf("restarted %d times on exit, want 1", f.starts)
	}
	if !f.running {
		t.Error("the service should be running again after the TUI exits")
	}
}

// -no-takeover must leave the service alone and surface the ordinary refusal.
func TestHandoverDisabledRefuses(t *testing.T) {
	dir := t.TempDir()
	held, err := acquireInstanceLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	f := &fakeService{running: true}
	withService(t, f.ctl())

	if _, _, err := acquireLockWithHandover(dir, false); err == nil {
		t.Fatal("expected a refusal when handover is disabled")
	}
	if f.stops != 0 {
		t.Error("a disabled handover must not touch the service")
	}
}

// Something we cannot account for holds the lock — another terminal, a stale
// hand-started process. Refusing is right; guessing and killing is not.
func TestHandoverRefusesWhenNoServiceInstalled(t *testing.T) {
	dir := t.TempDir()
	held, err := acquireInstanceLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	withService(t, nil) // no service manager on this machine

	_, _, err = acquireLockWithHandover(dir, true)
	if err == nil {
		t.Fatal("expected a refusal with no service to hand over from")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Errorf("error = %q, want the ordinary lock refusal", err)
	}
}

// The service is installed but stopped, so whatever holds the lock is not it.
func TestHandoverRefusesWhenServiceIsInactive(t *testing.T) {
	dir := t.TempDir()
	held, err := acquireInstanceLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	f := &fakeService{running: false}
	withService(t, f.ctl())

	if _, _, err := acquireLockWithHandover(dir, true); err == nil {
		t.Fatal("expected a refusal: an inactive service is not the lock holder")
	}
	if f.stops != 0 {
		t.Error("must not stop a service that is already inactive")
	}
}

func TestHandoverReportsStopFailure(t *testing.T) {
	dir := t.TempDir()
	held, err := acquireInstanceLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	f := &fakeService{running: true, stopErr: errors.New("permission denied")}
	withService(t, f.ctl())

	_, _, err = acquireLockWithHandover(dir, true)
	if err == nil {
		t.Fatal("expected an error when the service cannot be stopped")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("error = %q, want it to name the underlying failure", err)
	}
}

// If the lock never frees, put the service back. Leaving the user's bot
// stopped because we failed to start is strictly worse than not starting.
func TestHandoverRestartsServiceWhenLockNeverFrees(t *testing.T) {
	dir := t.TempDir()
	held, err := acquireInstanceLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	f := &fakeService{running: true}
	// stop() reports success but the lock is never released.
	withService(t, f.ctl())

	prev := lockWaitTimeout
	lockWaitTimeout = 200 * time.Millisecond
	t.Cleanup(func() { lockWaitTimeout = prev })

	_, _, err = acquireLockWithHandover(dir, true)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if f.starts != 1 {
		t.Errorf("restarted %d times, want 1 — the service must be put back", f.starts)
	}
}

// detectService must not report a manager on a machine that has none, and must
// never panic probing for one.
func TestDetectServiceIsSafeWithoutAManager(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no systemctl or launchctl anywhere
	if svc := detectService(); svc != nil {
		t.Errorf("detected %q with no service manager on PATH", svc.Name)
	}
}

func TestManualStartHintNamesARealCommand(t *testing.T) {
	hint := manualStartHint()
	if !strings.Contains(hint, "otto") {
		t.Errorf("hint = %q, want it to name Otto's service", hint)
	}
}

// Guard the assumption detectService relies on: `systemctl --user cat` is what
// distinguishes an installed unit from an absent one.
func TestSystemctlProbeShapeIsStable(t *testing.T) {
	if _, err := exec.LookPath("systemctl"); err != nil {
		t.Skip("no systemctl on this machine")
	}
	// A unit that certainly does not exist must fail the probe.
	if err := exec.Command("systemctl", "--user", "cat", "otto-does-not-exist.service").Run(); err == nil {
		t.Error("`systemctl --user cat` succeeded for a nonexistent unit; the probe would report a service that is not installed")
	}
}
