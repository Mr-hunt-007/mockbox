package watch

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// bump rewrites the file and moves its mtime forward so the change is
// visible even on filesystems with coarse timestamps.
func bump(t *testing.T, path, content string, offset time.Duration) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	ts := time.Now().Add(offset)
	if err := os.Chtimes(path, ts, ts); err != nil {
		t.Fatal(err)
	}
}

func TestChanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.json")
	bump(t, path, `{}`, -time.Hour)
	w := New(path)
	if w.Changed() {
		t.Fatal("no change yet")
	}
	bump(t, path, `{"a": []}`, time.Hour)
	if !w.Changed() {
		t.Fatal("modification not detected")
	}
	if w.Changed() {
		t.Fatal("same change reported twice")
	}
	// Same size, different mtime.
	bump(t, path, `{"b": []}`, 2*time.Hour)
	if !w.Changed() {
		t.Fatal("same-size modification not detected")
	}
}

func TestMissingFileAndReappearance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.json")
	bump(t, path, `{}`, 0)
	w := New(path)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if w.Changed() {
		t.Fatal("a missing file must not count as a change")
	}
	bump(t, path, `{}`, 0)
	if !w.Changed() {
		t.Fatal("reappearing file must count as a change")
	}
}

func TestRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.json")
	bump(t, path, `{}`, -time.Hour)
	w := New(path)
	var calls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		w.Run(ctx, 10*time.Millisecond, func() { calls.Add(1) })
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	bump(t, path, `{"x": 1}`, time.Hour)
	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if n := calls.Load(); n != 1 {
		t.Fatalf("onChange called %d times, want 1", n)
	}
}
