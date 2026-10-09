package dev

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// Session manages tasks and shutdown for a development session.
type Session struct {
	// WaitTimeout is the graceful-shutdown limit used by Wait and Catch. It
	// defaults to 10 seconds.
	WaitTimeout time.Duration

	ctx  context.Context
	stop context.CancelFunc

	mu        sync.Mutex
	closing   bool
	starts    sync.WaitGroup
	tasks     []*Task
	closeOnce sync.Once
	done      chan struct{}
}

// New creates a development session. The first interrupt or termination
// signal starts a graceful shutdown; a later one kills every managed process.
func New() *Session {
	ctx, stop := context.WithCancel(context.Background())
	session := &Session{WaitTimeout: 10 * time.Second, ctx: ctx, stop: stop, done: make(chan struct{})}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go session.handleSignals(signals)
	return session
}

func (session *Session) handleSignals(signals chan os.Signal) {
	defer signal.Stop(signals)
	for {
		select {
		case <-signals:
			if session.ctx.Err() == nil {
				session.stop()
				continue
			}
			// Managed processes run in their own process groups and miss the
			// terminal's signal, so kill them before restoring default handling.
			session.kill()
			return
		case <-session.done:
			return
		}
	}
}

func (session *Session) kill() {
	session.mu.Lock()
	tasks := append([]*Task(nil), session.tasks...)
	session.mu.Unlock()
	for _, task := range tasks {
		task.kill()
	}
}

// NewTask creates an unstarted task.
func (session *Session) NewTask(cmd Cmd) *Task {
	return &Task{
		session:   session,
		cmd:       cmd,
		done:      make(chan struct{}),
		ready:     make(chan struct{}),
		started:   make(chan struct{}),
		stopping:  make(chan struct{}),
		processes: make(map[*process]struct{}),
	}
}

// Run starts a Cmd when its Task handle is not needed.
func (session *Session) Run(cmd Cmd) error {
	return session.NewTask(cmd).Run()
}

func (session *Session) start() bool {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closing {
		return false
	}
	session.starts.Add(1)
	return true
}

func (session *Session) add(task *Task) bool {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closing {
		return false
	}
	session.tasks = append(session.tasks, task)
	return true
}

// Catch closes the development session and panics with err. It does nothing
// when err is nil.
func (session *Session) Catch(err error) {
	if err == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), session.WaitTimeout)
	defer cancel()
	if closeErr := session.Close(ctx); closeErr != nil {
		panic(errors.Join(err, closeErr))
	}
	panic(err)
}

// Wait blocks until the session receives an interrupt or termination signal,
// then shuts down the development session.
func (session *Session) Wait() error {
	<-session.ctx.Done()
	ctx, cancel := context.WithTimeout(context.Background(), session.WaitTimeout)
	defer cancel()
	return session.Close(ctx)
}

// Close shuts down the development session. It is safe to call more than once.
func (session *Session) Close(ctx context.Context) error {
	var result error
	session.closeOnce.Do(func() {
		session.stop()
		session.mu.Lock()
		session.closing = true
		session.mu.Unlock()
		// Let in-flight starts finish registration before collecting tasks.
		if err := session.waitStarts(ctx); err != nil {
			result = errors.Join(result, err)
		}

		// Snapshot the registered tasks before shutting them down concurrently.
		session.mu.Lock()
		tasks := append([]*Task(nil), session.tasks...)
		session.mu.Unlock()

		errorsByTask := make(chan error, len(tasks))
		var group sync.WaitGroup
		for _, task := range tasks {
			if task == nil {
				continue
			}
			group.Add(1)
			go func(task *Task) {
				defer group.Done()
				errorsByTask <- task.Close(ctx)
			}(task)
		}
		group.Wait()
		close(errorsByTask)
		var shutdownErrors []error
		for err := range errorsByTask {
			if err != nil {
				shutdownErrors = append(shutdownErrors, err)
			}
		}
		result = errors.Join(result, errors.Join(shutdownErrors...))
		close(session.done)
	})
	return result
}

func (session *Session) waitStarts(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		session.starts.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Done closes after the development session has shut down.
func (session *Session) Done() <-chan struct{} { return session.done }
