//go:build unix

package main

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"otto/internal/embed"
	"otto/internal/memory"
	"otto/internal/store"
)

func TestComposeMemoryPromptNilCoreReturnsBase(t *testing.T) {
	if got := composeMemoryPrompt("BASE", nil); got != "BASE" {
		t.Fatalf("nil core should return base unchanged, got %q", got)
	}
}

func TestComposeMemoryPromptEmptyCoreReturnsBase(t *testing.T) {
	c := memory.NewCore(t.TempDir(), 2200, 1375)
	if got := composeMemoryPrompt("BASE", c); got != "BASE" {
		t.Fatalf("empty core should return base unchanged, got %q", got)
	}
}

func TestComposeMemoryPromptAppendsCore(t *testing.T) {
	c := memory.NewCore(t.TempDir(), 2200, 1375)
	if err := c.Add(memory.TargetUser, "User is named Justin."); err != nil {
		t.Fatal(err)
	}
	got := composeMemoryPrompt("BASE PROMPT", c)
	if !strings.HasPrefix(got, "BASE PROMPT") {
		t.Errorf("base should come first: %q", got)
	}
	if !strings.Contains(got, "Justin") {
		t.Errorf("memory block missing: %q", got)
	}
}

func TestComposeMemoryPromptEmptyBaseReturnsBlockOnly(t *testing.T) {
	c := memory.NewCore(t.TempDir(), 2200, 1375)
	if err := c.Add(memory.TargetMemory, "Server runs Arch."); err != nil {
		t.Fatal(err)
	}
	got := composeMemoryPrompt("", c)
	if strings.HasPrefix(got, "\n") {
		t.Errorf("empty base should not leave a leading separator: %q", got)
	}
	if !strings.Contains(got, "Arch") {
		t.Errorf("memory block missing: %q", got)
	}
}

func TestLogTurnPersistsAndIsSearchable(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	logTurn(ctx, st, nil, "otto", "user", "remember the Tokyo trip")
	turns, err := st.SearchFTS(ctx, "Tokyo", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 {
		t.Fatalf("expected 1 logged turn, got %d", len(turns))
	}
}

func TestLogTurnNilStoreIsNoop(t *testing.T) {
	logTurn(context.Background(), nil, nil, "otto", "user", "anything")
}

func TestLogTurnSkipsBlankContent(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	logTurn(ctx, st, nil, "otto", "user", "   ")
	turns, err := st.SearchFTS(ctx, "anything", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 0 {
		t.Fatalf("blank content should not be logged, got %d turns", len(turns))
	}
}

// fakeEmbedder returns a fixed vector for any text.
type fakeEmbedder struct{ vec []float32 }

func (f fakeEmbedder) Embed(ctx context.Context, text string) (embed.Result, error) {
	return embed.Result{Vector: f.vec, Model: "fake"}, nil
}
func (f fakeEmbedder) Name() string { return "fake" }

func TestEmbedAndStorePersistsVector(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	id, err := st.AppendTurn(ctx, "otto", "user", "the Tokyo trip")
	if err != nil {
		t.Fatal(err)
	}
	embedAndStore(st, fakeEmbedder{vec: []float32{1, 0}}, id, "the Tokyo trip")

	got, err := st.SearchSemantic(ctx, []float32{1, 0}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != id {
		t.Fatalf("embedded turn not searchable: %+v", got)
	}
}

func TestCurrentTimeBlockFormatsLocalAndUTC(t *testing.T) {
	// Pin both the instant and the local zone so the assertion is deterministic
	// regardless of the host clock or TZ.
	prev := time.Local
	t.Cleanup(func() { time.Local = prev })
	loc := time.FixedZone("PDT", -7*3600)
	time.Local = loc

	fixed := time.Date(2026, 5, 31, 21, 32, 8, 0, time.UTC)
	got := currentTimeBlock(fixed)

	for _, want := range []string{
		"CURRENT TIME (sampled at this turn)",
		"Local:   Sun 2026-05-31 14:32:08 PDT (UTC-07:00)",
		"UTC:     2026-05-31 21:32:08",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("block missing %q\nfull block:\n%s", want, got)
		}
	}
}

func TestCurrentTimeBlockPositiveOffset(t *testing.T) {
	prev := time.Local
	t.Cleanup(func() { time.Local = prev })
	// Asia/Tokyo, no DST, fixed +09:00.
	time.Local = time.FixedZone("JST", 9*3600)

	fixed := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	got := currentTimeBlock(fixed)

	if !strings.Contains(got, "(UTC+09:00)") {
		t.Errorf("positive offset missing: %q", got)
	}
	if !strings.Contains(got, "Fri 2026-01-02 09:00:00 JST") {
		t.Errorf("local time wrong: %q", got)
	}
}

func TestComposePromptWithTimeAndMemoryIncludesTimeBlock(t *testing.T) {
	got := composePromptWithTimeAndMemory("BASE", nil)
	if !strings.HasPrefix(got, "BASE") {
		t.Errorf("base should come first: %q", got)
	}
	if !strings.Contains(got, "CURRENT TIME") {
		t.Errorf("time block missing: %q", got)
	}
}

func TestComposePromptWithTimeAndMemoryAlsoIncludesMemory(t *testing.T) {
	c := memory.NewCore(t.TempDir(), 2200, 1375)
	if err := c.Add(memory.TargetUser, "User is named Justin."); err != nil {
		t.Fatal(err)
	}
	got := composePromptWithTimeAndMemory("BASE", c)
	if !strings.Contains(got, "CURRENT TIME") {
		t.Errorf("time block missing: %q", got)
	}
	if !strings.Contains(got, "Justin") {
		t.Errorf("memory block missing: %q", got)
	}
	// Time block must precede the memory block.
	if strings.Index(got, "CURRENT TIME") > strings.Index(got, "Justin") {
		t.Errorf("time block should precede memory: %q", got)
	}
}

func TestComposePromptWithTimeAndMemoryEmptyBase(t *testing.T) {
	got := composePromptWithTimeAndMemory("", nil)
	if strings.HasPrefix(got, "\n") {
		t.Errorf("empty base should not leave a leading separator: %q", got)
	}
	if !strings.Contains(got, "CURRENT TIME") {
		t.Errorf("time block missing: %q", got)
	}
}

func TestLogTurnWithEmbedderStillLogsTurn(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	logTurn(ctx, st, fakeEmbedder{vec: []float32{1, 0}}, "otto", "user", "hello tokyo")
	if got, _ := st.SearchFTS(ctx, "tokyo", 5); len(got) == 0 {
		t.Fatal("turn not logged")
	}
	// logTurn embeds in a detached goroutine that writes the vector into
	// state.db. Wait for that write to land before returning; otherwise
	// t.TempDir cleanup races the goroutine and intermittently fails with
	// "directory not empty".
	deadline := time.Now().Add(2 * time.Second)
	for {
		if got, _ := st.SearchSemantic(ctx, []float32{1, 0}, 5); len(got) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("embed vector not persisted before timeout")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// serialEmbedder records whether two Embed calls overlapped in time, which
// would indicate the embedSem serialization is broken.
type serialEmbedder struct {
	mu      sync.Mutex
	active  atomic.Int32 // count of goroutines currently inside Embed
	overlap bool         // set if >1 was ever active simultaneously
	vec     []float32
}

func (s *serialEmbedder) Embed(ctx context.Context, text string) (embed.Result, error) {
	if s.active.Add(1) > 1 {
		s.mu.Lock()
		s.overlap = true
		s.mu.Unlock()
	}
	time.Sleep(10 * time.Millisecond) // hold long enough for a concurrent call to enter
	s.active.Add(-1)
	return embed.Result{Vector: s.vec, Model: "serial"}, nil
}
func (s *serialEmbedder) Name() string { return "serial" }

// TestEmbedAndStoreSerializesCallers verifies that two concurrent embedAndStore
// invocations do not overlap inside the embedder: the semaphore channel ensures
// only one embed runs at a time, preventing request pile-ups onto Ollama during
// a cold model load (which is exactly when the machine is busiest).
//
// The test uses embedAndStoreWithSem with a freshly-allocated semaphore so it
// is isolated from the package-level embedSem and from goroutines spawned by
// other tests (e.g. logTurn's detached goroutines).
func TestEmbedAndStoreSerializesCallers(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	id1, _ := st.AppendTurn(ctx, "otto", "user", "turn one")
	id2, _ := st.AppendTurn(ctx, "otto", "assistant", "turn two")

	emb := &serialEmbedder{vec: []float32{1, 0}}
	// A fresh sem guarantees this test is not affected by (or affecting) any
	// goroutine that holds the package-level embedSem.
	sem := make(chan struct{}, 1)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); embedAndStoreWithSem(st, emb, id1, "turn one", sem) }()
	go func() { defer wg.Done(); embedAndStoreWithSem(st, emb, id2, "turn two", sem) }()
	wg.Wait()

	if emb.overlap {
		t.Error("embedAndStore calls overlapped inside the embedder — semaphore serialization is broken")
	}
}
