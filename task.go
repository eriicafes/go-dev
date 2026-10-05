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

// Task controls a Cmd started by the Session. Tasks can reload.
// Tasks with ServerAddr also expose a stable proxy URL.
type Task struct {
	session *Session
	cmd     Cmd

	listener net.Listener
	http     *http.Server
	active   atomic.Pointer[process]

	reloadMu    sync.Mutex
	hooksMu     sync.Mutex
	hooks       []func(int)
	closeHooks  []func()
	middlewares []Middleware

	children []*Task

	closeOnce sync.Once
	readyOnce sync.Once
	closing   atomic.Bool
	done      chan struct{}
	ready     chan struct{}
	stopping  chan struct{}
	watcher   *watcher
}

type process struct {
	command *exec.Cmd
	done    chan struct{}
	url     *url.URL
	cleanup func()
}

func newTask(session *Session, cmd Cmd) (*Task, error) {
	task := &Task{
		session:  session,
		cmd:      cmd,
		done:     make(chan struct{}),
		ready:    make(chan struct{}),
		stopping: make(chan struct{}),
	}
	if task.cmd.ServerAddr == "" {
		return task, nil
	}
	listener, err := net.Listen("tcp", task.cmd.ServerAddr)
	if err != nil {
		return nil, fmt.Errorf("dev: listen %s: %w", task.cmd.ServerAddr, err)
	}
	task.listener = listener
	return task, nil
}

// Middleware wraps a task's stable proxy handler.
type Middleware func(http.Handler) http.Handler

// URL returns the task's stable proxy URL. It is empty when ServerAddr is not
// configured.
func (task *Task) URL() string {
	if task.listener == nil {
		return ""
	}
	return "http://" + task.listener.Addr().String()
}

// Use adds middleware around a task's stable proxy. It applies when the task
// configures ServerAddr.
func (task *Task) Use(middleware Middleware) {
	if task.cmd.ServerAddr != "" && middleware != nil {
		task.middlewares = append(task.middlewares, middleware)
	}
}

// OnReload registers a callback to run asynchronously after a process becomes
// live. The callback receives the process ID.
func (task *Task) OnReload(hook func(pid int)) {
	if hook == nil {
		return
	}
	task.hooksMu.Lock()
	defer task.hooksMu.Unlock()
	task.hooks = append(task.hooks, hook)
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
	task.cmd.WatchExclude = resolvePaths(dir, task.cmd.WatchExclude)
	if task.cmd.ServerAddr != "" && task.cmd.ServerHealthPath != "" && task.cmd.ServerHealthTimeout <= 0 {
		task.cmd.ServerHealthTimeout = 30 * time.Second
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
	if err := task.normalizeCmd(); err != nil {
		return err
	}
	if len(task.cmd.Watch) != 0 {
		// Configure source watching.
		task.watcher = newWatcher(&task.cmd, task.Reload)
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
	err := task.reload(task.session.ctx)
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
	child, err := newTask(task.session, config)
	if err != nil {
		return err
	}
	// Register the child before it starts so parent shutdown always includes it.
	task.children = append(task.children, child)
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
		if task.watcher != nil {
			// Stop a pending reload before taking the reload lock.
			task.watcher.close()
		}
		task.reloadMu.Lock()
		defer task.reloadMu.Unlock()
		close(task.stopping)
		// Stop accepting proxy requests before draining the active process.
		if task.http != nil {
			result = task.http.Shutdown(ctx)
		} else if task.listener != nil {
			result = task.listener.Close()
		}
		if current := task.active.Load(); current != nil {
			task.stopProcess(current, task.cmd.GracePeriod)
		}
		// Children share this task's lifetime.
		for _, child := range task.children {
			result = errors.Join(result, child.Close(ctx))
		}
		task.hooksMu.Lock()
		hooks := append([]func(){}, task.closeHooks...)
		task.hooksMu.Unlock()
		// Undo registered cleanup in reverse registration order.
		for _, hook := range slices.Backward(hooks) {
			hook()
		}
		close(task.done)
	})
	return result
}

// Reload starts a replacement process. Tasks with ServerAddr keep their
// current process live until the replacement is ready.
func (task *Task) Reload() error { return task.reload(task.session.ctx) }

func (task *Task) reload(ctx context.Context) error {
	// Serialize replacement creation and promotion.
	task.reloadMu.Lock()
	if task.closing.Load() {
		task.reloadMu.Unlock()
		return errors.New("dev: task is closing")
	}
	previous := task.active.Load()
	if previous != nil && task.cmd.ServerAddr == "" {
		// Without a proxy, the old process cannot coexist with its replacement.
		task.stopProcess(previous, task.cmd.GracePeriod)
	}
	next, err := task.startProcess(ctx)
	if err != nil {
		task.reloadMu.Unlock()
		return err
	}
	// Promote only a process that started and passed its optional health check.
	task.active.Store(next)
	task.readyOnce.Do(func() { close(task.ready) })
	task.hooksMu.Lock()
	hooks := append([]func(int){}, task.hooks...)
	task.hooksMu.Unlock()
	// Hooks may reload or close the task, so release the reload lock first.
	task.reloadMu.Unlock()
	if previous != nil && task.cmd.ServerAddr != "" {
		// The proxy now directs new requests to next while previous drains.
		go task.stopProcess(previous, task.cmd.GracePeriod)
	}
	for _, hook := range hooks {
		go hook(next.command.Process.Pid)
	}
	return nil
}

func (task *Task) startProcess(ctx context.Context) (*process, error) {
	// The target may create a temporary build artifact with matching cleanup.
	command, cleanup, err := task.cmd.Run.cmd(task.cmd)
	if err != nil {
		return nil, err
	}
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
	command.Args = append(command.Args, task.args()...)
	command.Dir = task.cmd.Dir
	command.Env = task.environment(port)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	prepareCommand(command)
	if err := command.Start(); err != nil {
		cleanup()
		return nil, fmt.Errorf("dev: start process: %w", err)
	}
	process := &process{command: command, done: make(chan struct{}), url: target, cleanup: cleanup}
	// Observe exit before waiting for health so an early exit ends the check.
	go task.waitProcess(process)
	if task.cmd.ServerAddr != "" && task.cmd.ServerHealthPath != "" {
		// Keep this process private until it reports readiness.
		if err := task.waitForHealth(ctx, process); err != nil {
			task.stopProcess(process, 0)
			return nil, fmt.Errorf("dev: process did not become healthy: %w", err)
		}
	}
	return process, nil
}

func (task *Task) waitProcess(process *process) {
	_ = process.command.Wait()
	// Clean up process artifacts.
	process.cleanup()
	close(process.done)
	task.reloadMu.Lock()
	current := task.active.Load() == process
	closing := task.closing.Load()
	watching := task.watcher != nil
	task.reloadMu.Unlock()
	if current && !closing && !watching {
		// Close the completed unwatched task.
		go func() { _ = task.Close(context.Background()) }()
	}
}

func (task *Task) waitForHealth(ctx context.Context, process *process) error {
	deadline := time.NewTimer(task.cmd.ServerHealthTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	path := task.cmd.ServerHealthPath
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	endpoint := *process.url
	endpoint.Path = path
	client := &http.Client{Timeout: 500 * time.Millisecond}
	for {
		// Retry non-ready responses until the process, caller, or deadline ends it.
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
		response, err := client.Do(request)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode >= 200 && response.StatusCode < 400 {
				return nil
			}
		}
		select {
		case <-process.done:
			return errors.New("process exited")
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("health check timed out")
		case <-ticker.C:
		}
	}
}

func (task *Task) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	for {
		current := task.active.Load()
		if current != nil {
			proxy := httputil.NewSingleHostReverseProxy(current.url)
			proxy.ErrorHandler = func(writer http.ResponseWriter, _ *http.Request, err error) {
				http.Error(writer, "server unavailable: "+err.Error(), http.StatusBadGateway)
			}
			proxy.ServeHTTP(writer, request)
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
	if grace <= 0 {
		_ = killProcess(process.command)
		return
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-process.done:
	case <-timer.C:
		_ = killProcess(process.command)
	}
}

func (task *Task) args() []string {
	args := append([]string(nil), task.cmd.Args...)
	args = append(args, os.Args[1:]...)
	return args
}

func (task *Task) environment(port string) []string {
	env := append(os.Environ(), task.cmd.Env...)
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
