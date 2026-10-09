package dev

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var errTaskClosing = errors.New("dev: task is closing")

// Task controls a Cmd started by the Session. Tasks can reload.
// Tasks with ServerAddr also expose a stable proxy URL.
type Task struct {
	session *Session
	cmd     Cmd

	listener net.Listener
	http     *http.Server
	active   atomic.Pointer[process]

	startMu     sync.Mutex
	reloadMu    sync.Mutex
	processesMu sync.Mutex
	processes   map[*process]struct{}
	hooksMu     sync.Mutex
	startHooks  []func(int)
	reloadHooks []func(int, []string)
	exitHooks   []func(int, error)
	closeHooks  []func()
	middlewares []Middleware

	childrenMu sync.Mutex
	children   []*Task

	runOnce sync.Once
	runErr  error

	closeOnce sync.Once
	closeErr  error
	readyOnce sync.Once
	closing   atomic.Bool
	forced    atomic.Bool
	exitErr   error
	done      chan struct{}
	ready     chan struct{}
	started   chan struct{}
	stopping  chan struct{}
	watcher   *watcher
}

type process struct {
	command *exec.Cmd
	done    chan struct{}
	proxy   *httputil.ReverseProxy
	cleanup func()
}

// Middleware wraps a task's stable proxy handler.
type Middleware func(http.Handler) http.Handler

// URL returns the task's stable proxy URL. It is empty when ServerAddr is not
// configured or the task has not started.
func (task *Task) URL() string {
	if task.listener == nil {
		return ""
	}
	host, port, err := net.SplitHostPort(task.listener.Addr().String())
	if err != nil {
		return "http://" + task.listener.Addr().String()
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// Use adds middleware around a task's stable proxy. It applies when the task
// configures ServerAddr and must be called before Run.
func (task *Task) Use(middleware Middleware) {
	if task.cmd.ServerAddr != "" && middleware != nil {
		task.middlewares = append(task.middlewares, middleware)
	}
}

// OnStart registers a callback to run asynchronously after the task's first
// process becomes live. The callback receives the process ID.
func (task *Task) OnStart(hook func(pid int)) {
	if hook == nil {
		return
	}
	task.hooksMu.Lock()
	defer task.hooksMu.Unlock()
	task.startHooks = append(task.startHooks, hook)
}

// OnReload registers a callback to run asynchronously after a replacement
// process becomes live. The callback receives the process ID and the deduped
// paths that caused the reload. Explicit reloads provide no paths.
func (task *Task) OnReload(hook func(pid int, paths []string)) {
	if hook == nil {
		return
	}
	task.hooksMu.Lock()
	defer task.hooksMu.Unlock()
	task.reloadHooks = append(task.reloadHooks, hook)
}

// OnProcessExit registers a callback to run asynchronously when a managed
// process exits. The callback receives the process ID and its exit error.
func (task *Task) OnProcessExit(hook func(pid int, err error)) {
	if hook == nil {
		return
	}
	task.hooksMu.Lock()
	defer task.hooksMu.Unlock()
	task.exitHooks = append(task.exitHooks, hook)
}

// OnClose registers cleanup to run when the task closes.
func (task *Task) OnClose(hook func()) {
	if hook == nil {
		return
	}
	task.hooksMu.Lock()
	defer task.hooksMu.Unlock()
	task.closeHooks = append(task.closeHooks, hook)
}

// Done closes when the task finishes.
func (task *Task) Done() <-chan struct{} { return task.done }

// Wait blocks until the task finishes. It returns the active process's exit
// error when that process ends before the task is closed.
func (task *Task) Wait() error {
	<-task.done
	return task.exitErr
}

// Run starts the task. Calling Run again returns the result of the first call.
func (task *Task) Run() error {
	task.runOnce.Do(func() {
		task.runErr = task.run()
	})
	return task.runErr
}

func (task *Task) run() error {
	if task.closing.Load() {
		return errTaskClosing
	}
	if !task.session.start() {
		return errors.New("dev: session is closed")
	}
	defer task.session.starts.Done()
	if err := task.start(); err != nil {
		_ = task.Close(context.Background())
		return err
	}
	if task.session.add(task) {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), task.session.WaitTimeout)
	defer cancel()
	return errors.Join(errors.New("dev: session is closed"), task.Close(ctx))
}

func (task *Task) normalizeCmd() error {
	if task.cmd.Run == nil {
		return errors.New("dev: Run is required")
	}
	if task.cmd.Dir == "" {
		dir, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("dev: get current directory: %w", err)
		}
		task.cmd.Dir = dir
	}
	dir, err := filepath.Abs(task.cmd.Dir)
	if err != nil {
		return fmt.Errorf("dev: resolve directory %q: %w", task.cmd.Dir, err)
	}
	task.cmd.Dir = dir
	task.cmd.Watch = resolvePaths(dir, task.cmd.Watch)
	task.cmd.WatchExclude = resolveExcludes(dir, task.cmd.WatchExclude)
	if task.cmd.ServerAddr != "" && task.cmd.ServerReadyTimeout <= 0 {
		task.cmd.ServerReadyTimeout = 10 * time.Second
	}
	if task.cmd.PollInterval <= 0 {
		task.cmd.PollInterval = 250 * time.Millisecond
	}
	if task.cmd.ReloadDelay <= 0 {
		task.cmd.ReloadDelay = 100 * time.Millisecond
	}
	if task.cmd.GracePeriod <= 0 {
		task.cmd.GracePeriod = 5 * time.Second
	}
	return nil
}

func (task *Task) start() error {
	// An early process exit waits for start to finish before closing the task.
	defer close(task.started)
	task.startMu.Lock()
	defer task.startMu.Unlock()
	if task.closing.Load() {
		return errTaskClosing
	}
	if err := task.normalizeCmd(); err != nil {
		return err
	}
	if task.cmd.ServerAddr != "" {
		listener, err := net.Listen("tcp", task.cmd.ServerAddr)
		if err != nil {
			return fmt.Errorf("dev: listen %s: %w", task.cmd.ServerAddr, err)
		}
		task.listener = listener
	}
	if len(task.cmd.Watch) != 0 {
		// Configure source watching.
		task.watcher = newWatcher(&task.cmd, task.reload)
	}
	for _, child := range task.cmd.Commands {
		if child.Phase < 0 {
			// Start before commands.
			if err := task.startChild(child); err != nil {
				return err
			}
		}
	}
	// Apply task plugins.
	for _, plugin := range task.cmd.Plugins {
		if plugin != nil {
			if err := plugin.Use(task); err != nil {
				return fmt.Errorf("dev: use plugin: %w", err)
			}
		}
	}
	if task.listener != nil {
		// Start the HTTP proxy.
		var handler http.Handler = http.HandlerFunc(task.serveHTTP)
		for _, middleware := range slices.Backward(task.middlewares) {
			handler = middleware(handler)
		}
		task.http = &http.Server{Handler: handler}
		go func() { _ = task.http.Serve(task.listener) }()
	}
	// Start zero-phase commands.
	started := make(chan error, 1)
	go func() {
		for _, child := range task.cmd.Commands {
			if child.Phase == 0 {
				if err := task.startChild(child); err != nil {
					started <- err
					return
				}
			}
		}
		started <- nil
	}()
	// Start the initial process.
	err := task.reload(nil)
	childErr := <-started
	if err != nil {
		return err
	}
	if childErr != nil {
		return childErr
	}
	for _, child := range task.cmd.Commands {
		if child.Phase > 0 {
			// Start after commands.
			if err := task.startChild(child); err != nil {
				return err
			}
		}
	}
	if task.watcher != nil {
		// Start source watching.
		task.watcher.start()
	}
	return nil
}

func (task *Task) startChild(config Cmd) error {
	if config.Dir == "" {
		config.Dir = task.cmd.Dir
	} else if !filepath.IsAbs(config.Dir) {
		// Child-relative directories resolve from the parent's working directory.
		config.Dir = filepath.Join(task.cmd.Dir, config.Dir)
	}
	child := task.session.NewTask(config)
	// Register the child before it starts so parent shutdown always includes it.
	task.childrenMu.Lock()
	if task.closing.Load() {
		task.childrenMu.Unlock()
		_ = child.Close(context.Background())
		return errTaskClosing
	}
	task.children = append(task.children, child)
	task.childrenMu.Unlock()
	if err := child.start(); err != nil {
		// A partially started child still owns resources that must be released.
		_ = child.Close(context.Background())
		return err
	}
	return nil
}

// Close stops the task and its child tasks. It is safe to call more than once.
func (task *Task) Close(ctx context.Context) error {
	var result error
	task.closeOnce.Do(func() {
		// Prevent new reloads before stopping work already in progress.
		task.closing.Store(true)
		// Keep startup from publishing resources while shutdown tears them down.
		task.startMu.Lock()
		defer task.startMu.Unlock()
		if task.watcher != nil {
			// Stop a pending reload before taking the reload lock.
			task.watcher.close()
		}
		task.reloadMu.Lock()
		defer task.reloadMu.Unlock()
		close(task.stopping)
		// Stop accepting proxy requests, then drain processes while in-flight
		// requests finish. Long-lived requests end when their process stops.
		var shutdown chan error
		if task.http != nil {
			shutdown = make(chan error, 1)
			go func() { shutdown <- task.http.Shutdown(ctx) }()
		} else if task.listener != nil {
			result = task.listener.Close()
		}
		task.stopProcesses(task.snapshotProcesses(), task.cmd.GracePeriod)
		if shutdown != nil {
			result = <-shutdown
		}
		// Children share this task's lifetime.
		task.childrenMu.Lock()
		children := slices.Clone(task.children)
		task.childrenMu.Unlock()
		for _, child := range children {
			result = errors.Join(result, child.Close(ctx))
		}
		task.hooksMu.Lock()
		hooks := append([]func(){}, task.closeHooks...)
		task.hooksMu.Unlock()
		// Undo registered cleanup in reverse registration order.
		for _, hook := range slices.Backward(hooks) {
			hook()
		}
		task.closeErr = result
		close(task.done)
	})
	return task.closeErr
}

// Reload starts a replacement process. Tasks with ServerAddr keep their
// current process live until the replacement is ready.
func (task *Task) Reload() error { return task.reload(nil) }

func (task *Task) reload(paths []string) error {
	// Serialize replacement creation and promotion.
	task.reloadMu.Lock()
	if task.closing.Load() {
		task.reloadMu.Unlock()
		return errTaskClosing
	}
	previous := task.active.Load()
	ctx, cancel := context.WithCancel(task.session.ctx)
	defer cancel()
	for _, hook := range task.cmd.Prepare {
		if hook == nil {
			continue
		}
		if err := hook.Prepare(ctx, task.cmd); err != nil {
			task.reloadMu.Unlock()
			return fmt.Errorf("dev: prepare: %w", err)
		}
	}
	// Prepare the replacement first so a failed build keeps the old process.
	// The target may create a temporary build artifact with matching cleanup.
	command, cleanup, err := task.cmd.Run.Cmd(task.cmd)
	if err != nil {
		task.reloadMu.Unlock()
		return err
	}
	if previous != nil && task.cmd.ServerAddr == "" {
		// Without a proxy, the old process cannot coexist with its replacement.
		task.stopProcess(previous, task.cmd.GracePeriod)
	}
	next, err := task.startProcess(task.session.ctx, command, cleanup)
	if err != nil {
		task.reloadMu.Unlock()
		return err
	}
	// Promote only a process that started and passed its readiness check.
	task.active.Store(next)
	task.readyOnce.Do(func() { close(task.ready) })
	task.hooksMu.Lock()
	var startHooks []func(int)
	var reloadHooks []func(int, []string)
	if previous == nil {
		startHooks = append(startHooks, task.startHooks...)
	} else {
		reloadHooks = append(reloadHooks, task.reloadHooks...)
	}
	task.hooksMu.Unlock()
	// Hooks may reload or close the task, so release the reload lock first.
	task.reloadMu.Unlock()
	if previous != nil && task.cmd.ServerAddr != "" {
		// The proxy now directs new requests to next while previous drains.
		go task.stopProcess(previous, task.cmd.GracePeriod)
	}
	for _, hook := range startHooks {
		go hook(next.command.Process.Pid)
	}
	for _, hook := range reloadHooks {
		go hook(next.command.Process.Pid, slices.Clone(paths))
	}
	return nil
}

func (task *Task) startProcess(ctx context.Context, command *exec.Cmd, cleanup func()) (*process, error) {
	port := ""
	var target *url.URL
	if task.cmd.ServerAddr != "" {
		// Give each proxied process an isolated loopback port.
		addr, err := availableAddr()
		if err != nil {
			cleanup()
			return nil, err
		}
		target, _ = url.Parse("http://" + addr)
		_, port, _ = net.SplitHostPort(addr)
	}
	command.Args = append(command.Args, task.cmd.Args...)
	command.Dir = task.cmd.Dir
	command.Env = task.environment(port)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	prepareCommand(command)
	if err := command.Start(); err != nil {
		cleanup()
		return nil, fmt.Errorf("dev: start process: %w", err)
	}
	process := &process{command: command, done: make(chan struct{}), cleanup: cleanup}
	if target != nil {
		process.proxy = httputil.NewSingleHostReverseProxy(target)
		process.proxy.ErrorHandler = func(writer http.ResponseWriter, _ *http.Request, err error) {
			http.Error(writer, "server unavailable: "+err.Error(), http.StatusBadGateway)
		}
	}
	task.processesMu.Lock()
	task.processes[process] = struct{}{}
	task.processesMu.Unlock()
	// Observe exit before waiting for readiness so an early exit ends the check.
	go task.waitProcess(process)
	if task.cmd.ServerAddr != "" {
		// Keep this process private until it reports readiness.
		if err := task.waitForReady(ctx, process, target); err != nil {
			task.stopProcess(process, 0)
			return nil, fmt.Errorf("dev: process did not become ready: %w", err)
		}
	}
	return process, nil
}

func (task *Task) waitProcess(process *process) {
	err := process.command.Wait()
	// Clean up process artifacts.
	process.cleanup()
	close(process.done)
	task.processesMu.Lock()
	delete(task.processes, process)
	task.processesMu.Unlock()
	task.hooksMu.Lock()
	hooks := append([]func(int, error){}, task.exitHooks...)
	task.hooksMu.Unlock()
	for _, hook := range hooks {
		go hook(process.command.Process.Pid, err)
	}
	task.reloadMu.Lock()
	current := task.active.Load() == process
	closing := task.closing.Load()
	watching := task.watcher != nil
	if current && !closing && !watching {
		task.exitErr = err
	}
	task.reloadMu.Unlock()
	if current && !closing && !watching {
		// Close the completed unwatched task once start has registered children.
		go func() {
			<-task.started
			_ = task.Close(context.Background())
		}()
	}
}

func (task *Task) waitForReady(ctx context.Context, process *process, target *url.URL) error {
	deadline := time.NewTimer(task.cmd.ServerReadyTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	ready := task.readinessCheck(target)
	for {
		// Retry until the process is ready or the process, caller, or deadline
		// ends the check.
		if ready(ctx) {
			return nil
		}
		select {
		case <-process.done:
			return errors.New("process exited")
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("readiness check timed out")
		case <-ticker.C:
		}
	}
}

func (task *Task) readinessCheck(target *url.URL) func(context.Context) bool {
	if task.cmd.ServerReadyPath == "" {
		// Without a ready path, a process is ready once its port accepts
		// connections.
		dialer := &net.Dialer{Timeout: 500 * time.Millisecond}
		return func(ctx context.Context) bool {
			conn, err := dialer.DialContext(ctx, "tcp", target.Host)
			if err != nil {
				return false
			}
			_ = conn.Close()
			return true
		}
	}
	path := task.cmd.ServerReadyPath
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	endpoint := *target
	endpoint.Path = path
	client := &http.Client{Timeout: 500 * time.Millisecond}
	return func(ctx context.Context) bool {
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
		response, err := client.Do(request)
		if err != nil {
			return false
		}
		_ = response.Body.Close()
		return response.StatusCode >= 200 && response.StatusCode < 400
	}
}

func (task *Task) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	for {
		current := task.active.Load()
		if current != nil {
			current.proxy.ServeHTTP(writer, request)
			return
		}
		select {
		case <-task.ready:
		case <-task.stopping:
			http.Error(writer, "server unavailable", http.StatusServiceUnavailable)
			return
		case <-request.Context().Done():
			return
		}
	}
}

func (task *Task) stopProcess(process *process, grace time.Duration) {
	select {
	case <-process.done:
		return
	default:
	}
	_ = interruptProcess(process.command)
	if grace <= 0 || task.forced.Load() {
		_ = killProcess(process.command)
		<-process.done
		return
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-process.done:
	case <-timer.C:
		_ = killProcess(process.command)
		<-process.done
	}
}

// kill immediately stops every process of the task and its children. Processes
// stopped later skip their grace period.
func (task *Task) kill() {
	task.forced.Store(true)
	for _, process := range task.snapshotProcesses() {
		select {
		case <-process.done:
		default:
			_ = killProcess(process.command)
		}
	}
	task.childrenMu.Lock()
	children := slices.Clone(task.children)
	task.childrenMu.Unlock()
	for _, child := range children {
		child.kill()
	}
}

func (task *Task) snapshotProcesses() []*process {
	task.processesMu.Lock()
	defer task.processesMu.Unlock()
	processes := make([]*process, 0, len(task.processes))
	for process := range task.processes {
		processes = append(processes, process)
	}
	return processes
}

func (task *Task) stopProcesses(processes []*process, grace time.Duration) {
	var group sync.WaitGroup
	for _, process := range processes {
		group.Go(func() {
			task.stopProcess(process, grace)
		})
	}
	group.Wait()
}

func (task *Task) environment(port string) []string {
	// A nil Env inherits the operating-system environment, like exec.Cmd.
	env := slices.Clone(task.cmd.Env)
	if env == nil {
		env = os.Environ()
	}
	if port != "" {
		env = append(env, "PORT="+port)
	}
	return env
}

func availableAddr() (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("dev: reserve app address: %w", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		return "", err
	}
	return addr, nil
}
