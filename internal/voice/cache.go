package voice

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// Pre-rendered audio for the phrases Otto says without consulting a model:
// wake acknowledgments, mute/unmute confirmations, and conversation closers.
//
// These are the phrases where latency is most obvious, because they are the
// ones answering "otto?" — a reply that takes a second and a half to say "yes?"
// feels broken in a way a slow answer to a real question does not. Rendering
// them once turns each into a file read plus playback, ~10 ms against ~1.5 s
// for a cold piper spawn.

// Cache stores rendered phrase audio on disk, keyed by voice *and* text.
//
// Keying on both is load-bearing now that each persona has its own voice: a
// text-only key would let Toto's "you got it" be served in Otto's voice by
// whichever character happened to say it first, which is exactly the confusion
// per-persona voices exist to prevent.
type Cache struct {
	Dir string
}

// NewCache returns a cache rooted under the voice directory.
func NewCache(voiceDir string) *Cache {
	return &Cache{Dir: filepath.Join(voiceDir, "phrase-cache")}
}

// Path returns the on-disk location for a (model, text) pair. The model's
// basename is used rather than its full path so moving the voice directory does
// not invalidate the cache.
func (c *Cache) Path(model, text string) string {
	sum := sha256.Sum256([]byte(filepath.Base(model) + "\x00" + text))
	return filepath.Join(c.Dir, hex.EncodeToString(sum[:8])+".wav")
}

// Get returns cached audio, or nil on any miss. Errors are deliberately
// collapsed into nil: every caller's fallback is live synthesis, so
// distinguishing "absent" from "unreadable" would only add a branch that does
// the same thing.
func (c *Cache) Get(model, text string) []byte {
	b, err := os.ReadFile(c.Path(model, text))
	if err != nil {
		return nil
	}
	return b
}

// Put stores rendered audio. Writes to a temp file and renames so a crash
// mid-write can never leave a truncated WAV that would later play as a click.
func (c *Cache) Put(model, text string, wav []byte) error {
	if err := os.MkdirAll(c.Dir, 0700); err != nil {
		return err
	}
	dst := c.Path(model, text)
	tmp, err := os.CreateTemp(c.Dir, ".phrase-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(wav); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// Warm renders every canned phrase for the given voice model that is not
// already cached. Idempotent, so repeated runs are nearly free.
//
// Intended to run in the background at startup: the first run costs a second or
// so per phrase, which is fine while the UI is booting, and every run after
// that is a series of stat calls.
func (c *Cache) Warm(ctx context.Context, sp Speaker, model string) error {
	if err := os.MkdirAll(c.Dir, 0700); err != nil {
		return err
	}
	for _, phrase := range CannedPhrases() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := os.Stat(c.Path(model, phrase)); err == nil {
			continue
		}
		wav, err := sp.Speak(ctx, phrase, model)
		if err != nil {
			return fmt.Errorf("warm %q: %w", phrase, err)
		}
		if err := c.Put(model, phrase, wav); err != nil {
			return fmt.Errorf("cache %q: %w", phrase, err)
		}
	}
	return nil
}
