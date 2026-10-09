package dev

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

var alwaysExcludedPaths = []string{".git", "node_modules"}

type watcher struct {
	cmd     *Cmd
	reload  func([]string) error
	stop    chan struct{}
	done    chan struct{}
	started bool
}

func newWatcher(cmd *Cmd, reload func([]string) error) *watcher {
	return &watcher{cmd: cmd, reload: reload, stop: make(chan struct{}), done: make(chan struct{})}
}

func (w *watcher) start() {
	w.started = true
	go w.watch()
}

func (w *watcher) close() {
	close(w.stop)
	if w.started {
		<-w.done
	}
}

func (w *watcher) watch() {
	defer close(w.done)
	// Alternate between two snapshots to avoid reallocating them every poll.
	previous := takeSnapshot(nil, w.cmd.Watch, w.cmd.WatchExclude)
	var next map[string]fileState
	ticker := time.NewTicker(w.cmd.PollInterval)
	defer ticker.Stop()
	var changed map[string]struct{}
	var timer *time.Timer
	var reload <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		select {
		case <-w.stop:
			return
		case <-ticker.C:
			next = takeSnapshot(next, w.cmd.Watch, w.cmd.WatchExclude)
			paths := changedPaths(previous, next)
			if len(paths) == 0 {
				continue
			}
			previous, next = next, previous
			if changed == nil {
				changed = make(map[string]struct{})
			}
			for _, path := range paths {
				changed[path] = struct{}{}
			}
			if timer == nil {
				timer = time.NewTimer(w.cmd.ReloadDelay)
				reload = timer.C
			} else {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(w.cmd.ReloadDelay)
			}
		case <-reload:
			reload = nil
			timer = nil
			paths := make([]string, 0, len(changed))
			for path := range changed {
				relative, err := filepath.Rel(w.cmd.Dir, path)
				if err == nil {
					path = relative
				}
				paths = append(paths, path)
			}
			slices.Sort(paths)
			changed = nil
			if err := w.reload(paths); err != nil && !errors.Is(err, errTaskClosing) {
				fmt.Fprintf(os.Stderr, "dev: reload: %v\n", err)
			}
		}
	}
}

type fileState struct {
	size    int64
	modTime time.Time
}

// takeSnapshot records the watched files in states, which it clears first and
// allocates when nil. Paths are filtered by name before they are stat'ed.
func takeSnapshot(states map[string]fileState, paths, excludes []string) map[string]fileState {
	if states == nil {
		states = make(map[string]fileState)
	}
	clear(states)
	for _, path := range paths {
		_ = filepath.WalkDir(path, func(name string, entry fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if isExcludedPath(name, excludes) {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if entry.IsDir() {
				if isExcludedPath(entry.Name(), alwaysExcludedPaths) {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(entry.Name(), "_test.go") {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return nil
			}
			states[name] = fileState{size: info.Size(), modTime: info.ModTime()}
			return nil
		})
	}
	return states
}

func changedPaths(left, right map[string]fileState) []string {
	var paths []string
	for path, state := range left {
		if next, ok := right[path]; !ok || next != state {
			paths = append(paths, path)
		}
	}
	for path := range right {
		if _, ok := left[path]; !ok {
			paths = append(paths, path)
		}
	}
	return paths
}

func isExcludedPath(path string, excludes []string) bool {
	for _, exclude := range excludes {
		if path == exclude || strings.HasPrefix(path, exclude+string(filepath.Separator)) {
			return true
		}
		if matches, _ := filepath.Match(exclude, path); matches {
			return true
		}
		// Patterns without a separator match names at any depth.
		if isBasePattern(exclude) {
			if matches, _ := filepath.Match(exclude, filepath.Base(path)); matches {
				return true
			}
		}
	}
	return false
}

func isBasePattern(pattern string) bool {
	return strings.ContainsAny(pattern, "*?[") && !strings.ContainsAny(pattern, "/"+string(filepath.Separator))
}

// resolveExcludes resolves exclusions like resolvePaths but keeps patterns
// without a separator, which match names at any depth.
func resolveExcludes(dir string, excludes []string) []string {
	resolved := resolvePaths(dir, excludes)
	for index, exclude := range excludes {
		if isBasePattern(exclude) {
			resolved[index] = exclude
		}
	}
	return resolved
}

func resolvePaths(dir string, paths []string) []string {
	resolved := append([]string(nil), paths...)
	for index, path := range resolved {
		if !filepath.IsAbs(path) {
			resolved[index] = filepath.Join(dir, path)
		}
	}
	return resolved
}
