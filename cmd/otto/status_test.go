//go:build unix

package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"otto/internal/store"
)

// newStatusTestStore opens a throwaway state.db for the status tests.
func newStatusTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestDescribeRotationDisabled(t *testing.T) {
	got := describeRotation(1000, time.Minute, false, rotateConfig{})
	if got != "disabled" {
		t.Errorf("got %q, want %q", got, "disabled")
	}
}

func TestDescribeRotationEmptySession(t *testing.T) {
	got := describeRotation(0, 0, true, testRotateConfig())
	if !strings.Contains(got, "no active session") {
		t.Errorf("got %q, want a no-active-session message", got)
	}
}

// A live session with no observed turn must not be reported as empty — they
// mean opposite things ("already cleared" vs "nothing measured yet").
func TestDescribeRotationSessionWithNoTurns(t *testing.T) {
	got := describeRotation(0, 0, false, testRotateConfig())
	if !strings.Contains(got, "no turn observed") {
		t.Errorf("got %q, want a no-turn-observed message", got)
	}
}

func TestDescribeRotationCountsDownToIdleReset(t *testing.T) {
	got := describeRotation(50000, 10*time.Minute, false, testRotateConfig())
	if !strings.Contains(got, "idle reset in 5m") {
		t.Errorf("got %q, want a 5m idle-reset countdown", got)
	}
	if !strings.Contains(got, "50000 tok") || !strings.Contains(got, "25%") {
		t.Errorf("got %q, want the token usage and percentage", got)
	}
}

// Past the hard threshold the grace window is shorter than the idle window, so
// the report must name the sooner of the two rather than always the idle path.
func TestDescribeRotationPrefersSoonerHardCap(t *testing.T) {
	got := describeRotation(180000, time.Minute, false, testRotateConfig())
	if !strings.Contains(got, "hard cap") {
		t.Errorf("got %q, want the hard cap named as the next trigger", got)
	}
	if !strings.Contains(got, "in 4m") {
		t.Errorf("got %q, want the 4m remaining grace", got)
	}
}

// Under the hard threshold the cap is irrelevant no matter how idle we are.
func TestDescribeRotationBelowHardCapUsesIdlePath(t *testing.T) {
	got := describeRotation(1000, 6*time.Minute, false, testRotateConfig())
	if strings.Contains(got, "hard cap") {
		t.Errorf("got %q, hard cap should not apply below the threshold", got)
	}
	if !strings.Contains(got, "idle reset in 9m") {
		t.Errorf("got %q, want a 9m idle-reset countdown", got)
	}
}

// When rotation is due the report must say so without implying it has already
// happened — the rotator still has to win the Otto slot.
func TestDescribeRotationDueNow(t *testing.T) {
	got := describeRotation(50000, 20*time.Minute, false, testRotateConfig())
	if !strings.Contains(got, "due now") {
		t.Errorf("got %q, want a due-now report", got)
	}
	if !strings.Contains(got, "Otto is free") {
		t.Errorf("got %q, want the slot caveat", got)
	}
}

func TestEmbedTrackerUnexercised(t *testing.T) {
	var tr embedTracker
	got := tr.describe()
	if !strings.Contains(got, "not exercised yet") {
		t.Errorf("got %q, want an explicit not-yet-tested report", got)
	}
	if strings.Contains(got, "ok") {
		t.Errorf("got %q, must not imply health before anything ran", got)
	}
}

func TestEmbedTrackerRecordsSuccessAndFailure(t *testing.T) {
	var tr embedTracker

	tr.record(nil)
	if got := tr.describe(); !strings.HasPrefix(got, "ok (") {
		t.Errorf("after success got %q, want an ok report", got)
	}

	tr.record(errors.New("connection refused"))
	got := tr.describe()
	if !strings.Contains(got, "degraded") || !strings.Contains(got, "connection refused") {
		t.Errorf("after failure got %q, want a degraded report naming the error", got)
	}

	// Recovery must clear the stale error text, not append to it.
	tr.record(nil)
	if got := tr.describe(); strings.Contains(got, "connection refused") {
		t.Errorf("got %q, a recovered embedder must not still report the old error", got)
	}
}

func TestPruneTrackerReports(t *testing.T) {
	var tr pruneTracker
	if got := tr.describe(); got != "not run yet" {
		t.Errorf("got %q, want %q", got, "not run yet")
	}

	tr.record(42, false)
	if got := tr.describe(); !strings.Contains(got, "42 rows removed") {
		t.Errorf("got %q, want the removal count", got)
	}

	tr.record(0, true)
	if got := tr.describe(); !strings.Contains(got, "errors") {
		t.Errorf("got %q, want a failure to be visible", got)
	}
}

// statusReport must render every line even with no store wired, and must never
// panic on a partially-constructed handler.
func TestStatusReportWithoutStore(t *testing.T) {
	h := &handler{
		session:   newTestSession(t, "sid", ""),
		otto:      newOttoState(),
		startedAt: time.Now(),
		rotate:    testRotateConfig(),
	}
	got := h.statusReport(context.Background())
	for _, want := range []string{"uptime=", "state=idle", "model=", "session=", "rotation=", "embeddings=", "prune=", "bus=disabled"} {
		if !strings.Contains(got, want) {
			t.Errorf("status missing %q:\n%s", want, got)
		}
	}
}

// An unreadable inbox must report as unavailable rather than as empty — the two
// mean opposite things and "bus=empty" would be an actively misleading answer.
func TestStatusReportSurfacesInboxDepth(t *testing.T) {
	st := newStatusTestStore(t)
	h := &handler{
		session:   newTestSession(t, "sid", ""),
		otto:      newOttoState(),
		startedAt: time.Now(),
		rotate:    testRotateConfig(),
		store:     st,
	}

	if got := h.statusReport(context.Background()); !strings.Contains(got, "bus=empty") {
		t.Errorf("with no queued rows got %q, want bus=empty", got)
	}

	if _, err := st.Enqueue(context.Background(), "otto", "agent", "toto", "hello", 1); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	got := h.statusReport(context.Background())
	if !strings.Contains(got, "bus=1 queued, 1 ready now") {
		t.Errorf("got %q, want a one-queued-one-ready report", got)
	}
}
