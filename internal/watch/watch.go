// Package watch detects file changes by polling modification time and size.
// Polling works the same on every OS and survives editors that save by
// writing a new file and renaming it over the old one.
package watch

import (
	"context"
	"os"
	"time"
)

// Watcher remembers the last seen state of one file.
type Watcher struct {
	path    string
	modTime time.Time
	size    int64
	exists  bool
}

// New records the current state of path.
func New(path string) *Watcher {
	w := &Watcher{path: path}
	w.modTime, w.size, w.exists = stat(path)
	return w
}

func stat(path string) (time.Time, int64, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}, 0, false
	}
	return info.ModTime(), info.Size(), true
}

// Changed reports whether the file changed since the last call. A missing
// file is not a change (editors briefly remove files while saving); its
// reappearance is.
func (w *Watcher) Changed() bool {
	mt, size, ok := stat(w.path)
	if !ok {
		w.exists = false
		return false
	}
	changed := !w.exists || !mt.Equal(w.modTime) || size != w.size
	w.modTime, w.size, w.exists = mt, size, true
	return changed
}

// Run polls every interval until ctx is done, calling onChange after each change.
func (w *Watcher) Run(ctx context.Context, interval time.Duration, onChange func()) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if w.Changed() {
				onChange()
			}
		}
	}
}
