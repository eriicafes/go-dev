package dev

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

type watcher struct {
	cmd     *Cmd
	reload  func() error
	stop    chan struct{}
	done    chan struct{}
	started atomic.Bool
}

func newWatcher(cmd *Cmd, reload func() error) *watcher {
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
	previous := takeSnapshot(w.cmd.Watch)
	ticker := time.NewTicker(w.cmd.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-w.stop:
			return
		case <-ticker.C:
			next := takeSnapshot(w.cmd.Watch)
			if snapshotsEqual(previous, next) {
				continue
			}
			previous = next
			timer := time.NewTimer(w.cmd.ReloadDelay)
			select {
			case <-w.stop:
				timer.Stop()
				return
			case <-timer.C:
				_ = w.reload()
			}
		}
	}
}

type fileState struct {
	size    int64
	modTime time.Time
}

func takeSnapshot(paths []string) map[string]fileState {
	states := make(map[string]fileState)
	for _, path := range paths {
		_ = filepath.Walk(path, func(name string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				if info != nil && info.IsDir() && (info.Name() == ".git" || info.Name() == "node_modules") {
					return filepath.SkipDir
				}
				return nil
			}
			states[name] = fileState{size: info.Size(), modTime: info.ModTime()}
			return nil
		})
	}
	return states
}

func snapshotsEqual(left, right map[string]fileState) bool {
	if len(left) != len(right) {
		return false
	}
	for name, state := range left {
		if right[name] != state {
			return false
		}
	}
	return true
}
