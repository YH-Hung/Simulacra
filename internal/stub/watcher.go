package stub

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

// WatchOptions configures the internal context-aware watcher used by serve.
type WatchOptions struct {
	Debounce time.Duration
	OnChange func(context.Context)
	OnError  func(error)
	Ready    func()
}

// Watch monitors dirs recursively and calls onChange after changes have been
// quiet for debounce. Calls are serial and coalesced while a callback runs.
func Watch(ctx context.Context, dirs []string, debounce time.Duration, onChange func()) error {
	if onChange == nil {
		return errors.New("watch callback must not be nil")
	}
	return WatchWithOptions(ctx, dirs, WatchOptions{
		Debounce: debounce,
		OnChange: func(context.Context) { onChange() },
	})
}

// WatchWithOptions is Watch's context-aware form. Cancellation stops event
// collection immediately even when an OnChange call has not returned.
func WatchWithOptions(ctx context.Context, dirs []string, opts WatchOptions) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("create watcher: %w", err)
	}
	return watchWithBackend(ctx, dirs, opts, fsnotifyBackend{w})
}

type watchBackend interface {
	Add(string) error
	Remove(string) error
	Events() <-chan fsnotify.Event
	Errors() <-chan error
	Close() error
}

type fsnotifyBackend struct{ *fsnotify.Watcher }

func (w fsnotifyBackend) Events() <-chan fsnotify.Event { return w.Watcher.Events }
func (w fsnotifyBackend) Errors() <-chan error          { return w.Watcher.Errors }

type watchState struct {
	backend    watchBackend
	roots      []string
	persistent map[string]struct{}
	watched    map[string]struct{}
	report     func(error)
}

func watchWithBackend(ctx context.Context, dirs []string, opts WatchOptions, backend watchBackend) error {
	return watchWithBackendHooks(ctx, dirs, opts, backend, watchWorkerHooks{})
}

type watchWorkerHooks struct {
	started func()
	stopped func()
}

func watchWithBackendHooks(ctx context.Context, dirs []string, opts WatchOptions, backend watchBackend, hooks watchWorkerHooks) error {
	if opts.OnChange == nil {
		_ = backend.Close()
		return errors.New("watch callback must not be nil")
	}
	if opts.Debounce <= 0 {
		_ = backend.Close()
		return fmt.Errorf("watch debounce must be greater than zero (got %s)", opts.Debounce)
	}
	defer backend.Close()
	if opts.OnError == nil {
		opts.OnError = func(error) {}
	}

	roots, err := normalizeRoots(dirs)
	if err != nil {
		return err
	}
	state := &watchState{
		backend:    backend,
		roots:      roots,
		persistent: make(map[string]struct{}),
		watched:    make(map[string]struct{}),
		report:     opts.OnError,
	}
	if err := state.initialAttach(); err != nil {
		return err
	}

	if opts.Ready != nil {
		opts.Ready()
	}
	if ctx.Err() != nil {
		return nil
	}
	workerCtx, cancelWorker := context.WithCancel(ctx)
	defer cancelWorker()
	reloads := make(chan struct{}, 1)
	callbackErrs := make(chan error, 1)
	startCallbackWorker(workerCtx, reloads, callbackErrs, opts.OnChange, hooks)

	reloadTimer := time.NewTimer(opts.Debounce)
	stopTimer(reloadTimer)
	retryTimer := time.NewTimer(opts.Debounce)
	stopTimer(retryTimer)
	defer stopTimer(reloadTimer)
	defer stopTimer(retryTimer)
	var reloadC, retryC <-chan time.Time
	resetReload := func() {
		resetTimer(reloadTimer, opts.Debounce)
		reloadC = reloadTimer.C
	}
	resetRetry := func() {
		resetTimer(retryTimer, opts.Debounce)
		retryC = retryTimer.C
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-backend.Events():
			if !ok {
				return nil
			}
			if event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) == 0 || !state.relevant(event.Name) {
				continue
			}
			if event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
				state.forgetTree(event.Name)
				// Schedule a reconcile: the path may already have been
				// replaced. An atomic swap (rename the directory away, then
				// recreate it) emits a Rename and a Create in one rescan
				// batch, and their relative order is not guaranteed. If the
				// Create was processed first, its reconcile ran while the
				// stale watch was still registered and did nothing — and the
				// forget above has just dropped that watch. Reconciling after
				// the debounce re-attaches either way; without it the root
				// stays unwatched for the life of the process and hot reload
				// silently stops.
				resetRetry()
			}
			if event.Op&fsnotify.Create != 0 && state.reconcile(false) {
				resetRetry()
			}
			resetReload()
		case watcherErr, ok := <-backend.Errors():
			if !ok {
				return nil
			}
			state.safeReport(watcherErr)
			if state.reconcile(true) {
				resetRetry()
			}
			resetReload()
		case callbackErr := <-callbackErrs:
			state.safeReport(callbackErr)
		case <-retryC:
			retryC = nil
			if state.reconcile(false) {
				resetRetry()
			}
		case <-reloadC:
			reloadC = nil
			select {
			case reloads <- struct{}{}:
			default:
			}
		}
	}
}

func startCallbackWorker(ctx context.Context, reloads <-chan struct{}, callbackErrs chan<- error, onChange func(context.Context), hooks watchWorkerHooks) {
	if hooks.started != nil {
		hooks.started()
	}
	go func() {
		if hooks.stopped != nil {
			defer hooks.stopped()
		}
		callbackWorker(ctx, reloads, callbackErrs, onChange)
	}()
}

func callbackWorker(ctx context.Context, reloads <-chan struct{}, callbackErrs chan<- error, onChange func(context.Context)) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-reloads:
			if ctx.Err() != nil {
				continue
			}
			func() {
				defer func() {
					if recovered := recover(); recovered != nil {
						err := fmt.Errorf("watch callback panic: %v\n%s", recovered, debug.Stack())
						select {
						case callbackErrs <- err:
						case <-ctx.Done():
						}
					}
				}()
				onChange(ctx)
			}()
		}
	}
}

func normalizeRoots(dirs []string) ([]string, error) {
	roots := make([]string, 0, len(dirs))
	seen := make(map[string]struct{})
	for _, dir := range dirs {
		root, err := filepath.Abs(dir)
		if err != nil {
			return nil, fmt.Errorf("resolve watch root %s: %w", dir, err)
		}
		root = filepath.Clean(root)
		if _, ok := seen[root]; ok {
			continue
		}
		seen[root] = struct{}{}
		roots = append(roots, root)
	}
	return roots, nil
}

func (s *watchState) initialAttach() error {
	for _, root := range s.roots {
		parent := filepath.Dir(root)
		s.persistent[parent] = struct{}{}
		if err := s.addDirectory(parent); err != nil {
			return fmt.Errorf("watch root parent %s: %w", parent, err)
		}
		if err := s.addRecursive(root); err != nil {
			return err
		}
	}
	return nil
}

func (s *watchState) relevant(name string) bool {
	path, err := filepath.Abs(name)
	if err != nil {
		return false
	}
	path = filepath.Clean(path)
	for _, root := range s.roots {
		if path == root || strings.HasPrefix(path, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// reconcile restores the desired root watches. It returns true when a
// non-disappearance error requires another retry.
func (s *watchState) reconcile(force bool) bool {
	if force {
		for path := range s.watched {
			_ = s.backend.Remove(path)
			delete(s.watched, path)
		}
	}
	retry := false
	for parent := range s.persistent {
		if err := s.addDirectory(parent); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				s.safeReport(err)
				retry = true
			}
		}
	}
	for _, root := range s.roots {
		info, err := os.Stat(root)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				s.forgetTree(root)
				continue
			}
			s.safeReport(fmt.Errorf("inspect watch root %s: %w", root, err))
			retry = true
			continue
		}
		if !info.IsDir() {
			s.safeReport(fmt.Errorf("watch root %s is not a directory", root))
			retry = true
			continue
		}
		if err := s.addRecursive(root); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			s.safeReport(err)
			retry = true
		}
	}
	return retry
}

func (s *watchState) addRecursive(root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			return nil
		}
		return s.addDirectory(filepath.Clean(path))
	})
}

func (s *watchState) addDirectory(path string) error {
	if _, ok := s.watched[path]; ok {
		return nil
	}
	if err := s.backend.Add(path); err != nil {
		return fmt.Errorf("watch directory %s: %w", path, err)
	}
	s.watched[path] = struct{}{}
	return nil
}

func (s *watchState) forgetTree(root string) {
	root, err := filepath.Abs(root)
	if err != nil {
		return
	}
	root = filepath.Clean(root)
	prefix := root + string(filepath.Separator)
	for path := range s.watched {
		if _, keep := s.persistent[path]; keep {
			continue
		}
		if path == root || strings.HasPrefix(path, prefix) {
			_ = s.backend.Remove(path)
			delete(s.watched, path)
		}
	}
}

func (s *watchState) safeReport(err error) {
	if err == nil {
		return
	}
	defer func() { _ = recover() }()
	s.report(err)
}

func resetTimer(timer *time.Timer, delay time.Duration) {
	stopTimer(timer)
	timer.Reset(delay)
}

func stopTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}
