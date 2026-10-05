package dev

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

var alwaysExcludedPaths = []string{".git", "node_modules"}

type watcher struct {
	cmd     *Cmd
	reload  func([]string) error
	stop    chan struct{}
	done    chan struct{}
	started atomic.Bool
}

func newWatcher(cmd *Cmd, reload func([]string) error) *watcher {
	return &watcher{cmd: cmd, reload: reload, stop: make(chan struct{}), done: make(chan struct{})}
}

func (w *watcher) start() {
	// Mark started before launching the goroutine so close waits for it.
	w.started.Store(true)
	go w.watch()
}

func (w *watcher) close() {
	close(w.stop)
	if w.started.Load() {
		<-w.done
	}
}

func (w *watcher) watch() {
	defer close(w.done)
	previous := takeSnapshot(w.cmd.Watch, w.cmd.WatchExclude)
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
			next := takeSnapshot(w.cmd.Watch, w.cmd.WatchExclude)
			paths := changedPaths(previous, next)
			if len(paths) == 0 {
				continue
			}
			previous = next
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
			_ = w.reload(paths)
		}
	}
}

type fileState struct {
	size    int64
	modTime time.Time
}

func takeSnapshot(paths, excludes []string) map[string]fileState {
	states := make(map[string]fileState)
	for _, path := range paths {
		_ = filepath.Walk(path, func(name string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			if isExcludedPath(name, excludes) {
				if info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if info.IsDir() {
				if isExcludedPath(info.Name(), alwaysExcludedPaths) {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(info.Name(), "_test.go") {
				return nil
			}
			states[name] = fileState{size: info.Size(), modTime: info.ModTime()}
			return nil
		})
	}
	return states
}

func changedPaths(left, right map[string]fileState) []string {
	changed := make(map[string]struct{})
	for path, state := range left {
		if right[path] != state {
			changed[path] = struct{}{}
		}
	}
	for path, state := range right {
		if left[path] != state {
			changed[path] = struct{}{}
		}
	}
	paths := make([]string, 0, len(changed))
	for path := range changed {
		paths = append(paths, path)
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
	}
	return false
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
