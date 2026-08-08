package voice

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os/exec"
	"sync"
)

// The microphone, and the latch that turns it off.
//
// The original loop held one sox process open for the life of the session and
// tried to reason its way out of hearing itself: the pre-roll ring was cleared
// during playback, only a narrow list of phrases was allowed to interrupt, and
// each captured utterance carried the state speech *started* in so loopback
// could be recognized after the fact. All of that machinery existed to answer
// one question — "is this Otto's own voice?" — and none of it answered it
// reliably, because the honest answer depends on room acoustics rather than on
// anything the transcript contains.
//
// So the device is released instead. While Otto is thinking or speaking there is
// no sox process, no PCM, and nothing to misclassify. The question stops being
// hard by ceasing to be asked.

// CaptureDevice streams microphone audio as frames of 16 kHz mono PCM.
//
// It is an interface for one reason: gating the device on and off is now the
// central design of this package, and a real sound card cannot be opened in a
// test. SoxCapture is the only production implementation.
type CaptureDevice interface {
	// Capture opens the device and writes frames to out until ctx is cancelled,
	// releasing the device before it returns. Returning is the signal that the
	// microphone is genuinely closed, so an implementation must not return
	// while its capture process is still alive.
	//
	// It must not close out — the caller reuses the channel across sessions.
	Capture(ctx context.Context, out chan<- []int16) error

	// Available reports why the device cannot be opened, or nil when it can.
	// Checked once at startup so a missing dependency is a clear message rather
	// than a capture loop that fails forever in the background.
	Available() error
}

// SoxCapture reads the default capture device via the sox CLI.
type SoxCapture struct{}

// Available reports whether sox is installed.
func (SoxCapture) Available() error {
	if _, err := exec.LookPath("sox"); err != nil {
		return fmt.Errorf("sox not installed — run ./setup.sh, or `otto voice-doctor` for details")
	}
	return nil
}

// Capture runs sox until ctx is cancelled, then waits for it to die.
//
// The wait is the part that matters. exec.CommandContext kills the process when
// the context is cancelled, but returning before it has been reaped would let
// the caller flip the UI to "microphone off" while sox still owned the device —
// which is exactly the lie this whole design exists to avoid.
func (SoxCapture) Capture(ctx context.Context, out chan<- []int16) error {
	// Raw signed 16-bit little-endian mono PCM at 16 kHz on stdout.
	cmd := exec.CommandContext(ctx, "sox",
		"-q", "-d",
		"-c", "1",
		"-r", fmt.Sprint(sampleRate),
		"-b", "16",
		"-e", "signed-integer",
		"-L",
		"-t", "raw",
		"-",
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start sox: %w", err)
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()

	buf := make([]byte, frameSamples*2)
	for {
		if ctx.Err() != nil {
			return nil
		}
		if _, err := io.ReadFull(stdout, buf); err != nil {
			// A cancelled context kills sox, which shows up here as a read
			// error. That is the ordinary way this function ends, not a fault.
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("read capture: %w", err)
		}
		frame := make([]int16, frameSamples)
		for i := range frame {
			frame[i] = int16(binary.LittleEndian.Uint16(buf[i*2 : i*2+2]))
		}
		select {
		case out <- frame:
		case <-ctx.Done():
			return nil
		}
	}
}

// ─── The latch ───────────────────────────────────────────────────────────

// micGate is a two-way latch between the conversation state machine and the
// capture loop. The state machine flips it (via setState); the capture loop
// waits on it to know when to open the device and when to release it.
//
// Both directions are exposed as channels rather than a condition variable so
// the capture loop can select on the gate and on context cancellation together,
// which a sync.Cond cannot do.
type micGate struct {
	mu      sync.Mutex
	open    bool
	onOpen  chan struct{} // closed while open
	onClose chan struct{} // closed while closed
}

// newMicGate returns a gate that starts closed, so nothing can record before
// the state machine has said it may.
func newMicGate() *micGate {
	g := &micGate{
		onOpen:  make(chan struct{}),
		onClose: make(chan struct{}),
	}
	close(g.onClose)
	return g
}

// Open lets the capture loop start the device. Idempotent.
func (g *micGate) Open() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.open {
		return
	}
	g.open = true
	g.onClose = make(chan struct{})
	close(g.onOpen)
}

// Close tells the capture loop to release the device. Idempotent.
func (g *micGate) Close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.open {
		return
	}
	g.open = false
	g.onOpen = make(chan struct{})
	close(g.onClose)
}

// Opened returns a channel closed while the gate is open.
func (g *micGate) Opened() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.onOpen
}

// Closed returns a channel closed while the gate is closed.
func (g *micGate) Closed() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.onClose
}

// IsOpen reports the current position of the latch.
func (g *micGate) IsOpen() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.open
}
