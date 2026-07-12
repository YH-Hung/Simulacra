// Package watch monitors directory trees for changes.
package watch

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watch monitors dirs recursively and calls onChange after changes have been
// quiet for debounce. The callback runs serially in Watch's event loop.
func Watch(ctx context.Context, dirs []string, debounce time.Duration, onChange func()) error {
	if onChange == nil {
		return errors.New("watch callback must not be nil")
	}
	if debounce <= 0 {
		return fmt.Errorf("watch debounce must be greater than zero (got %s)", debounce)
	}

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("create watcher: %w", err)
	}
	defer w.Close()

	watched := make(map[string]struct{})
	for _, dir := range dirs {
		if err := addRecursive(w, watched, dir); err != nil {
			return err
		}
	}

	timer := time.NewTimer(debounce)
	if !timer.Stop() {
		<-timer.C
	}
	defer stopTimer(timer)
	var timerC <-chan time.Time

	reset := func() {
		stopTimer(timer)
		timer.Reset(debounce)
		timerC = timer.C
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-w.Events:
			if !ok {
				return nil
			}
			if event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) == 0 {
				continue
			}
			if event.Op&fsnotify.Create != 0 {
				info, statErr := os.Stat(event.Name)
				if statErr == nil && info.IsDir() {
					if err := addRecursive(w, watched, event.Name); err != nil {
						return err
					}
				}
			}
			if event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
				forgetTree(w, watched, event.Name)
			}
			reset()
		case _, ok := <-w.Errors:
			if !ok {
				return nil
			}
			// Filesystem watcher errors can be transient. Keep monitoring; a
			// later file event will still schedule the reload.
		case <-timerC:
			timerC = nil
			onChange()
		}
	}
}

func addRecursive(w *fsnotify.Watcher, watched map[string]struct{}, root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			return nil
		}
		clean := filepath.Clean(path)
		if _, ok := watched[clean]; ok {
			return nil
		}
		if err := w.Add(clean); err != nil {
			return fmt.Errorf("watch directory %s: %w", clean, err)
		}
		watched[clean] = struct{}{}
		return nil
	})
}

func forgetTree(w *fsnotify.Watcher, watched map[string]struct{}, root string) {
	root = filepath.Clean(root)
	prefix := root + string(filepath.Separator)
	for path := range watched {
		if path == root || strings.HasPrefix(path, prefix) {
			_ = w.Remove(path)
			delete(watched, path)
		}
	}
}

func stopTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}
