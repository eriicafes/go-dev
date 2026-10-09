package dev

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestReloadSwapsOnlyAfterReplacementIsReady(t *testing.T) {
	file := filepath.Join(t.TempDir(), "app.go")
	writeGoApp(t, file, "ok")
	live := make(chan int, 2)
	server := newSession(t)
	task := server.NewTask(Cmd{
		GracePeriod:        10 * time.Millisecond,
		Run:                Package(file),
		ServerAddr:         "127.0.0.1:3000",
		ServerReadyPath:    "/",
		ServerReadyTimeout: 10 * time.Second,
		Plugins: Plugins(PluginFunc(func(task *Task) error {
			onLive := func(pid int) { live <- pid }
			task.OnStart(onLive)
			task.OnReload(func(pid int, _ []string) { onLive(pid) })
			return nil
		})),
	})
	if err := task.Run(); err != nil {
		t.Fatal(err)
	}
	assertBody(t, task.URL(), "ok")
	firstPID := <-live
	if firstPID <= 0 {
		t.Fatalf("first live pid = %d, want positive", firstPID)
	}
	if err := task.Reload(); err != nil {
		t.Fatal(err)
	}
	assertBody(t, task.URL(), "ok")
	if got := <-live; got <= 0 || got == firstPID {
		t.Fatalf("replacement live pid = %d, want a new positive pid", got)
	}
}

func TestFailedReloadLeavesCurrentProcessLive(t *testing.T) {
	file := filepath.Join(t.TempDir(), "app.go")
	writeGoApp(t, file, "one")
	server := newSession(t)
	task := server.NewTask(Cmd{Run: Package(file),
		ServerAddr:         "127.0.0.1:3000",
		ServerReadyPath:    "/",
		ServerReadyTimeout: 10 * time.Second,
	})
	if err := task.Run(); err != nil {
		t.Fatal(err)
	}
	writeFailGoApp(t, file)
	if err := task.Reload(); err == nil {
		t.Fatal("Reload unexpectedly succeeded")
	}
	assertBody(t, task.URL(), "one")
}

func TestEmptyReadyPathSkipsHTTPChecks(t *testing.T) {
	file := filepath.Join(t.TempDir(), "app.go")
	writeNotReadyGoApp(t, file)
	server := newSession(t)
	task := server.NewTask(Cmd{Run: Package(file), ServerAddr: "127.0.0.1:0", ServerReadyTimeout: 5 * time.Second})
	if err := task.Run(); err != nil {
		t.Fatalf("Task.Run with an empty ServerReadyPath = %v, want success", err)
	}
}

func TestEmptyReadyPathWaitsForPort(t *testing.T) {
	file := filepath.Join(t.TempDir(), "app.go")
	source := `package main
import (
    "net/http"
    "os"
    "time"
)
func main() {
    time.Sleep(300 * time.Millisecond)
    _ = http.ListenAndServe("127.0.0.1:"+os.Getenv("PORT"), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
        _, _ = w.Write([]byte("ok"))
    }))
}`
	if err := os.WriteFile(file, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	server := newSession(t)
	task := server.NewTask(Cmd{Run: Package(file), ServerAddr: "127.0.0.1:0"})
	if err := task.Run(); err != nil {
		t.Fatal(err)
	}
	assertBody(t, task.URL(), "ok")
	if err := task.Reload(); err != nil {
		t.Fatal(err)
	}
	assertBody(t, task.URL(), "ok")
}

func TestEmptyReadyPathTimesOutWithoutPort(t *testing.T) {
	file := filepath.Join(t.TempDir(), "app.go")
	writeWorker(t, file)
	server := newSession(t)
	task := server.NewTask(Cmd{Run: Package(file), ServerAddr: "127.0.0.1:0", ServerReadyTimeout: 2 * time.Second})
	if err := task.Run(); err == nil {
		t.Fatal("Task.Run succeeded for a process that never listens")
	}
}

func TestCmdWithoutServerDoesNotCreateProxy(t *testing.T) {
	file := filepath.Join(t.TempDir(), "app.go")
	writeWorker(t, file)
	server := newSession(t)
	task := server.NewTask(Cmd{Run: Package(file)})
	if err := task.Run(); err != nil {
		t.Fatal(err)
	}
	if got := task.URL(); got != "" {
		t.Fatalf("worker URL = %q, want empty", got)
	}
}

func TestURLNormalizesUnspecifiedHost(t *testing.T) {
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	task := &Task{listener: listener}
	if got, want := task.URL(), "http://localhost:"+port; got != want {
		t.Fatalf("URL = %q, want %q", got, want)
	}
}

func TestUseBeforeRunAppliesMiddleware(t *testing.T) {
	file := filepath.Join(t.TempDir(), "app.go")
	writeGoApp(t, file, "ok")
	server := newSession(t)
	task := server.NewTask(Cmd{Run: Package(file), ServerAddr: "127.0.0.1:0"})
	task.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set("X-Go-Dev", "middleware")
			next.ServeHTTP(writer, request)
		})
	})
	if err := task.Run(); err != nil {
		t.Fatal(err)
	}
	response, err := http.Get(task.URL())
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if got := response.Header.Get("X-Go-Dev"); got != "middleware" {
		t.Fatalf("middleware header = %q, want middleware", got)
	}
}

func TestNewTaskDefersAndRunsOnce(t *testing.T) {
	server := newSession(t)
	calls := 0
	task := server.NewTask(Cmd{Run: TargetFunc(func(Cmd) (*exec.Cmd, func(), error) {
		calls++
		return exec.Command(os.Args[0], "-test.run=^$"), func() {}, nil
	})})
	if calls != 0 {
		t.Fatalf("target calls before Run = %d, want 0", calls)
	}
	if err := task.Run(); err != nil {
		t.Fatal(err)
	}
	if err := task.Run(); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("target calls after two Run calls = %d, want 1", calls)
	}
}

func TestCmdWatchRestartsCommand(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "worker.go")
	marker := filepath.Join(dir, "pid")
	writeWatchCmd(t, file)
	binary := buildWatchCmd(t, dir, file)
	server := newSession(t)
	task := server.NewTask(Cmd{
		Run:   Binary(binary),
		Args:  Values(marker),
		Dir:   dir,
		Watch: Values(file),
	})
	if err := task.Run(); err != nil {
		t.Fatal(err)
	}
	firstPID := waitForPID(t, marker, 0)
	if err := task.Reload(); err != nil {
		t.Fatal(err)
	}
	secondPID := waitForPID(t, marker, firstPID)
	time.Sleep(300 * time.Millisecond)
	content, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, append(content, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := waitForPID(t, marker, secondPID); got == secondPID {
		t.Fatalf("reloaded command pid = %d, want a new pid", got)
	}
}

func TestCmdReloadKeepsTaskRunning(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "worker.go")
	marker := filepath.Join(dir, "pid")
	writeWatchCmd(t, file)
	binary := buildWatchCmd(t, dir, file)
	server := newSession(t)
	task := server.NewTask(Cmd{Run: Binary(binary), Args: Values(marker), Dir: dir})
	if err := task.Run(); err != nil {
		t.Fatal(err)
	}
	firstPID := waitForPID(t, marker, 0)
	if err := task.Reload(); err != nil {
		t.Fatal(err)
	}
	if got := waitForPID(t, marker, firstPID); got == firstPID {
		t.Fatalf("reloaded command pid = %d, want a new pid", got)
	}
	select {
	case <-task.Done():
		t.Fatal("task finished after its command reloaded")
	default:
	}
}

func TestCmdReloadsBehindProxy(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "server.go")
	writeCommandServer(t, file, "ok")
	binary := buildWatchCmd(t, dir, file)
	server := newSession(t)
	task := server.NewTask(Cmd{
		Run:                Binary(binary),
		Dir:                dir,
		ServerAddr:         "127.0.0.1:0",
		ServerReadyPath:    "/",
		ServerReadyTimeout: 5 * time.Second,
	})
	if err := task.Run(); err != nil {
		t.Fatal(err)
	}
	assertBody(t, task.URL(), "ok")
	first := task.active.Load()
	if err := task.Reload(); err != nil {
		t.Fatal(err)
	}
	if next := task.active.Load(); next == first {
		t.Fatal("command reload did not replace the active process")
	}
	assertBody(t, task.URL(), "ok")
}

func TestCmdClosesChildTasks(t *testing.T) {
	dir := t.TempDir()
	appFile := filepath.Join(dir, "app.go")
	commandFile := filepath.Join(dir, "worker.go")
	marker := filepath.Join(dir, "pid")
	writeWorker(t, appFile)
	writeWatchCmd(t, commandFile)
	binary := buildWatchCmd(t, dir, commandFile)
	server := newSession(t)
	task := server.NewTask(Cmd{
		Run: Package(appFile),
		Commands: Commands(Cmd{
			Run:  Binary(binary),
			Args: Values(marker),
		}),
	})
	if err := task.Run(); err != nil {
		t.Fatal(err)
	}
	_ = waitForPID(t, marker, 0)
	children := task.children
	if len(children) != 1 {
		t.Fatalf("child tasks = %d, want 1", len(children))
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := task.Close(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-children[0].Done():
	default:
		t.Fatal("child task did not close with its app")
	}
}

func TestTaskCloseWaitsForReplacedProcessCleanup(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "worker.go")
	marker := filepath.Join(dir, "starts")
	writeStubbornWorker(t, file)
	binary := buildWatchCmd(t, dir, file)
	cleaned := make(chan struct{}, 2)
	server := newSession(t)
	task := server.NewTask(Cmd{
		Run: cleanupTarget{
			Target:  Binary(binary),
			cleaned: cleaned,
		},
		Args:        Values(marker),
		Dir:         dir,
		ServerAddr:  "127.0.0.1:0",
		GracePeriod: 100 * time.Millisecond,
	})
	if err := task.Run(); err != nil {
		t.Fatal(err)
	}
	waitForStart(t, marker, 1)
	if err := task.Reload(); err != nil {
		t.Fatal(err)
	}
	waitForStart(t, marker, 2)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := task.Close(ctx); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		select {
		case <-cleaned:
		default:
			t.Fatal("Close returned before every process cleanup ran")
		}
	}
}

func TestReloadHookCanReload(t *testing.T) {
	file := filepath.Join(t.TempDir(), "app.go")
	writeWorker(t, file)
	server := newSession(t)
	reloadDone := make(chan error, 1)
	var once sync.Once
	task := server.NewTask(Cmd{
		Run: Package(file),
		Plugins: Plugins(PluginFunc(func(task *Task) error {
			task.OnReload(func(int, []string) {
				once.Do(func() { reloadDone <- task.Reload() })
			})
			return nil
		})),
	})
	if err := task.Run(); err != nil {
		t.Fatal(err)
	}
	if err := task.Reload(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-reloadDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("reload hook did not finish")
	}
}

func TestProcessExitHook(t *testing.T) {
	file := filepath.Join(t.TempDir(), "app.go")
	if err := os.WriteFile(file, []byte("package main\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	type exit struct {
		pid int
		err error
	}
	exited := make(chan exit, 1)
	server := newSession(t)
	task := server.NewTask(Cmd{
		Run: Package(file),
		Plugins: Plugins(PluginFunc(func(task *Task) error {
			task.OnProcessExit(func(pid int, err error) {
				exited <- exit{pid: pid, err: err}
			})
			return nil
		})),
	})
	if err := task.Run(); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-exited:
		if got.pid <= 0 {
			t.Fatalf("exit pid = %d, want positive", got.pid)
		}
		if got.err != nil {
			t.Fatalf("exit error = %v, want nil", got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("process exit hook did not run")
	}
}

func TestTaskWaitReturnsProcessExitError(t *testing.T) {
	file := filepath.Join(t.TempDir(), "app.go")
	if err := os.WriteFile(file, []byte("package main\nimport \"os\"\nfunc main() { os.Exit(1) }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	task := newSession(t).NewTask(Cmd{Run: Package(file)})
	if err := task.Run(); err != nil {
		t.Fatal(err)
	}
	if err := task.Wait(); err == nil {
		t.Fatal("Wait error = nil, want process exit error")
	}
	if err := task.Wait(); err == nil {
		t.Fatal("second Wait error = nil, want process exit error")
	}
}

func TestProxyWaitsForFirstProcess(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte("ok"))
	}))
	defer backend.Close()
	target, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	task := &Task{ready: make(chan struct{}), stopping: make(chan struct{})}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "http://proxy.test/", nil)
	finished := make(chan struct{})
	go func() {
		task.serveHTTP(response, request)
		close(finished)
	}()
	select {
	case <-finished:
		t.Fatal("proxy responded before an app process was live")
	case <-time.After(20 * time.Millisecond):
	}
	task.active.Store(&process{proxy: httputil.NewSingleHostReverseProxy(target)})
	task.readyOnce.Do(func() { close(task.ready) })
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("proxy did not resume after an app process became live")
	}
	if got, want := response.Body.String(), "ok"; got != want {
		t.Fatalf("proxy body = %q, want %q", got, want)
	}
}

func TestPluginFailureClosesEarlierPluginResources(t *testing.T) {
	server := newSession(t)
	closed := make(chan struct{})
	task := server.NewTask(Cmd{
		Run:        Binary("unused"),
		ServerAddr: "127.0.0.1:0",
		Plugins: Plugins(
			PluginFunc(func(task *Task) error {
				task.OnClose(func() { close(closed) })
				return nil
			}),
			PluginFunc(func(*Task) error { return errors.New("plugin failed") }),
		),
	})
	if err := task.Run(); err == nil {
		t.Fatal("Start unexpectedly succeeded")
	}
	select {
	case <-closed:
	default:
		t.Fatal("earlier plugin cleanup did not run")
	}
}

func TestStartFailureReturnsWithoutWaitingForWatcher(t *testing.T) {
	file := filepath.Join(t.TempDir(), "app.go")
	writeFailGoApp(t, file)
	finished := make(chan error, 1)
	go func() {
		server := New()
		task := server.NewTask(Cmd{Run: Package(file),
			ServerAddr:         "127.0.0.1:3000",
			ServerReadyPath:    "/",
			ServerReadyTimeout: 10 * time.Second,
		})
		err := task.Run()
		server.stop()
		finished <- err
	}()
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("Task.Run unexpectedly succeeded")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Task.Run did not return after its app failed")
	}
}

func TestWatchBuildsAndServesLatestSource(t *testing.T) {
	watchPath := t.TempDir()
	file := filepath.Join(watchPath, "app.go")
	writeGoApp(t, file, "one")
	live := make(chan int, 2)
	paths := make(chan []string, 1)
	server := newSession(t)
	task := server.NewTask(Cmd{
		Dir:             watchPath,
		Watch:           Values("."),
		PollInterval:    10 * time.Millisecond,
		ReloadDelay:     5 * time.Millisecond,
		GracePeriod:     10 * time.Millisecond,
		Run:             Package(file),
		ServerAddr:      "127.0.0.1:3000",
		ServerReadyPath: "/",
		Plugins: Plugins(PluginFunc(func(task *Task) error {
			onLive := func(pid int) { live <- pid }
			task.OnStart(onLive)
			task.OnReload(func(pid int, changed []string) {
				onLive(pid)
				paths <- changed
			})
			return nil
		})),
	})
	if err := task.Run(); err != nil {
		t.Fatal(err)
	}
	firstPID := <-live
	if firstPID <= 0 {
		t.Fatalf("first live pid = %d, want positive", firstPID)
	}
	assertBody(t, task.URL(), "one")

	// Give the watcher time to take its initial snapshot before changing a file.
	time.Sleep(30 * time.Millisecond)
	writeGoApp(t, file, "two")
	select {
	case got := <-live:
		if got <= 0 || got == firstPID {
			t.Fatalf("watched reload pid = %d, want a new positive pid", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watch did not trigger a reload")
	}
	if got, want := <-paths, []string{"app.go"}; !slices.Equal(got, want) {
		t.Fatalf("reload paths = %q, want %q", got, want)
	}
	assertBody(t, task.URL(), "two")
}

func TestValuesAndPair(t *testing.T) {
	got := Values(Pair("PORT", "1234"), Pair("LOG_LEVEL", "debug"))
	want := []string{"PORT=1234", "LOG_LEVEL=debug"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("environment = %#v, want %#v", got, want)
	}
}

func TestOsEnvAndGetenv(t *testing.T) {
	t.Setenv("GO_DEV_SET", "value")
	if got, want := OsEnv("APP_ENV=test"), append(os.Environ(), "APP_ENV=test"); !slices.Equal(got, want) {
		t.Fatalf("OsEnv = %q, want %q", got, want)
	}
	if got, want := Getenv("GO_DEV_SET", "GO_DEV_UNSET"), []string{"GO_DEV_SET=value"}; !slices.Equal(got, want) {
		t.Fatalf("Getenv = %q, want %q", got, want)
	}
}

func TestTaskEnvironment(t *testing.T) {
	t.Setenv("GO_DEV_SET", "value")
	inherited := (&Task{}).environment("1234")
	if !slices.Contains(inherited, "GO_DEV_SET=value") || inherited[len(inherited)-1] != "PORT=1234" {
		t.Fatalf("nil Env environment = %q, want inherited variables and PORT", inherited)
	}
	exact := (&Task{cmd: Cmd{Env: Values("APP_ENV=test")}}).environment("1234")
	if want := []string{"APP_ENV=test", "PORT=1234"}; !slices.Equal(exact, want) {
		t.Fatalf("explicit Env environment = %q, want %q", exact, want)
	}
	if empty := (&Task{cmd: Cmd{Env: []string{}}}).environment(""); len(empty) != 0 {
		t.Fatalf("empty Env environment = %q, want empty", empty)
	}
}

func TestDirResolvesFromCaller(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not find test source location")
	}
	if got, want := Dir("."), filepath.Dir(file); got != want {
		t.Fatalf("Dir(\".\") = %q, want %q", got, want)
	}
	if got, want := Dir("one", "two"), filepath.Join(filepath.Dir(file), "one", "two"); got != want {
		t.Fatalf("Dir(\"one\", \"two\") = %q, want %q", got, want)
	}
}

func TestCmdDirResolvesWatchPaths(t *testing.T) {
	dir := t.TempDir()
	task := &Task{cmd: Cmd{
		Run:          Binary("app"),
		Dir:          dir,
		Watch:        Values("app", filepath.Join(dir, "absolute")),
		WatchExclude: Values("generated", "*.generated.go", filepath.Join(dir, "vendor")),
	}}
	if err := task.normalizeCmd(); err != nil {
		t.Fatal(err)
	}
	if got, want := task.cmd.Watch[0], filepath.Join(dir, "app"); got != want {
		t.Fatalf("relative watch path = %q, want %q", got, want)
	}
	if got, want := task.cmd.Watch[1], filepath.Join(dir, "absolute"); got != want {
		t.Fatalf("absolute watch path = %q, want %q", got, want)
	}
	if got, want := task.cmd.WatchExclude[0], filepath.Join(dir, "generated"); got != want {
		t.Fatalf("relative exclude path = %q, want %q", got, want)
	}
	if got, want := task.cmd.WatchExclude[1], "*.generated.go"; got != want {
		t.Fatalf("relative exclude pattern = %q, want %q", got, want)
	}
	if got, want := task.cmd.WatchExclude[2], filepath.Join(dir, "vendor"); got != want {
		t.Fatalf("absolute exclude path = %q, want %q", got, want)
	}
}

func TestSnapshotExcludesTestFilesAndPaths(t *testing.T) {
	dir := t.TempDir()
	keep := filepath.Join(dir, "app.go")
	testFile := filepath.Join(dir, "app_test.go")
	globbed := filepath.Join(dir, "app.generated.go")
	nested := filepath.Join(dir, "sub", "app.generated.go")
	excluded := filepath.Join(dir, "generated")
	if err := os.WriteFile(keep, []byte("app"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(testFile, []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(globbed, []byte("generated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Dir(nested), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nested, []byte("generated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(excluded, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(excluded, "app.go"), []byte("generated"), 0o600); err != nil {
		t.Fatal(err)
	}
	states := takeSnapshot(nil, Values(dir), Values(excluded, "*.generated.go"))
	if len(states) != 1 {
		t.Fatalf("snapshot files = %d, want 1", len(states))
	}
	if _, ok := states[keep]; !ok {
		t.Fatalf("snapshot did not include %q", keep)
	}
}

func TestChangedPathsDedupesChanges(t *testing.T) {
	left := map[string]fileState{
		"changed": {size: 1},
		"removed": {size: 1},
	}
	right := map[string]fileState{
		"changed": {size: 2},
		"added":   {size: 1},
	}
	got := changedPaths(left, right)
	slices.Sort(got)
	if want := []string{"added", "changed", "removed"}; !slices.Equal(got, want) {
		t.Fatalf("changed paths = %q, want %q", got, want)
	}
}

func TestEmptyCmdDirUsesCurrentDirectory(t *testing.T) {
	want, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	task := &Task{cmd: Cmd{Run: Binary("app")}}
	if err := task.normalizeCmd(); err != nil {
		t.Fatal(err)
	}
	if task.cmd.Dir != want {
		t.Fatalf("default Cmd.Dir = %q, want %q", task.cmd.Dir, want)
	}
}

func TestCommandsPreservePhases(t *testing.T) {
	pre := Cmd{Run: Binary("vite"), Phase: Before}
	post := Cmd{Run: Binary("worker"), Dir: "tools", Phase: After}
	commands := Commands(pre, post)
	if len(commands) != 2 || commands[0].Phase != Before || commands[1].Phase != After {
		t.Fatalf("command phases = %#v", commands)
	}
}

func TestWaitClosesServerAfterSignalContextCancellation(t *testing.T) {
	server := New()
	result := make(chan error, 1)
	go func() { result <- server.Wait() }()
	select {
	case err := <-result:
		t.Fatalf("Wait returned before cancellation: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	server.stop()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	select {
	case <-server.Done():
	default:
		t.Fatal("server Done did not close")
	}
}

func TestTaskCloseReturnsInitialError(t *testing.T) {
	task := taskWithClosedListener(t)
	first := task.Close(context.Background())
	if !errors.Is(first, net.ErrClosed) {
		t.Fatalf("first Close error = %v, want closed listener error", first)
	}
	if second := task.Close(context.Background()); !errors.Is(second, net.ErrClosed) {
		t.Fatalf("second Close error = %v, want closed listener error", second)
	}
}

func TestSessionCloseReturnsInitialError(t *testing.T) {
	server := New()
	task := taskWithClosedListener(t)
	if !server.add(task) {
		t.Fatal("add task to open session")
	}
	first := server.Close(context.Background())
	if !errors.Is(first, net.ErrClosed) {
		t.Fatalf("first Close error = %v, want closed listener error", first)
	}
	if second := server.Close(context.Background()); !errors.Is(second, net.ErrClosed) {
		t.Fatalf("second Close error = %v, want closed listener error", second)
	}
}

func TestCloseRejectsTaskThatFinishesStarting(t *testing.T) {
	server := New()
	started := make(chan struct{})
	release := make(chan struct{})
	result := make(chan error, 1)
	task := server.NewTask(Cmd{Run: blockingTarget{started: started, release: release}})
	go func() {
		result <- task.Run()
	}()
	<-started

	closed := make(chan error, 1)
	go func() { closed <- server.Close(context.Background()) }()
	deadline := time.Now().Add(time.Second)
	for !sessionIsClosing(server) {
		if time.Now().After(deadline) {
			t.Fatal("server did not begin closing")
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	if err := <-result; err == nil {
		t.Fatal("Task.Run unexpectedly succeeded after server shutdown")
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}

func taskWithClosedListener(t *testing.T) *Task {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return &Task{
		listener:  listener,
		done:      make(chan struct{}),
		stopping:  make(chan struct{}),
		processes: make(map[*process]struct{}),
	}
}

type blockingTarget struct {
	started chan<- struct{}
	release <-chan struct{}
}

type cleanupTarget struct {
	Target
	cleaned chan<- struct{}
}

func (target cleanupTarget) Cmd(config Cmd) (*exec.Cmd, func(), error) {
	command, cleanup, err := target.Target.Cmd(config)
	if err != nil {
		return nil, nil, err
	}
	return command, func() {
		cleanup()
		target.cleaned <- struct{}{}
	}, nil
}

func (command blockingTarget) Cmd(Cmd) (*exec.Cmd, func(), error) {
	close(command.started)
	<-command.release
	return exec.Command(os.Args[0]), func() {}, nil
}

func sessionIsClosing(server *Session) bool {
	server.mu.Lock()
	defer server.mu.Unlock()
	return server.closing
}

func newSession(t *testing.T) *Session {
	t.Helper()
	server := New()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Close(ctx)
	})
	return server
}

func writeGoApp(t *testing.T, file, message string) {
	t.Helper()
	source := `package main
import (
    "fmt"
    "net/http"
    "os"
)
func main() {
    _ = http.ListenAndServe("127.0.0.1:" + os.Getenv("PORT"), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
        _, _ = fmt.Fprint(w, ` + strconv.Quote(message) + `)
    }))
}`
	if err := os.WriteFile(file, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeFailGoApp(t *testing.T, file string) {
	t.Helper()
	if err := os.WriteFile(file, []byte("package main\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeNotReadyGoApp(t *testing.T, file string) {
	t.Helper()
	source := `package main
import (
    "net/http"
    "os"
)
func main() {
    _ = http.ListenAndServe("127.0.0.1:" + os.Getenv("PORT"), http.NotFoundHandler())
}`
	if err := os.WriteFile(file, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeWorker(t *testing.T, file string) {
	t.Helper()
	source := `package main
import "time"
func main() { for { time.Sleep(time.Hour) } }
`
	if err := os.WriteFile(file, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeWatchCmd(t *testing.T, file string) {
	t.Helper()
	source := `package main
import (
    "os"
    "strconv"
    "time"
)
func main() {
    _ = os.WriteFile(os.Args[1], []byte(strconv.Itoa(os.Getpid())), 0o600)
    for { time.Sleep(time.Hour) }
}
`
	if err := os.WriteFile(file, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeStubbornWorker(t *testing.T, file string) {
	t.Helper()
	source := `package main
import (
    "net"
    "os"
    "os/signal"
    "strconv"
    "time"
)
func main() {
    listener, _ := net.Listen("tcp", "127.0.0.1:"+os.Getenv("PORT"))
    defer listener.Close()
    marker := os.Args[1]
    count, _ := strconv.Atoi(string(must(os.ReadFile(marker))))
    count++
    _ = os.WriteFile(marker, []byte(strconv.Itoa(count)), 0o600)
    if count == 1 {
        signals := make(chan os.Signal, 1)
        signal.Notify(signals, os.Interrupt)
        <-signals
    }
    for { time.Sleep(time.Hour) }
}
func must(bytes []byte, err error) []byte { return bytes }
`
	if err := os.WriteFile(file, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeCommandServer(t *testing.T, file, message string) {
	t.Helper()
	source := `package main
import (
    "fmt"
    "net/http"
    "os"
)
func main() {
    _ = http.ListenAndServe("127.0.0.1:" + os.Getenv("PORT"), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
        _, _ = fmt.Fprint(w, ` + strconv.Quote(message) + `)
    }))
}`
	if err := os.WriteFile(file, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
}

func buildWatchCmd(t *testing.T, dir, file string) string {
	t.Helper()
	binary := filepath.Join(dir, "worker")
	command := exec.Command("go", "build", "-o", binary, file)
	command.Dir = dir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build watched command: %v\n%s", err, output)
	}
	return binary
}

func waitForPID(t *testing.T, path string, previous int) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		contents, err := os.ReadFile(path)
		if err == nil {
			pid, err := strconv.Atoi(string(contents))
			if err == nil && pid > 0 && pid != previous {
				return pid
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for command pid different from %d", previous)
	return 0
}

func waitForStart(t *testing.T, path string, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		contents, err := os.ReadFile(path)
		if err == nil {
			if got, err := strconv.Atoi(string(contents)); err == nil && got >= want {
				return
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d process starts", want)
}

func assertBody(t *testing.T, endpoint, want string) {
	t.Helper()
	response, err := http.Get(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(body); got != want {
		t.Fatalf("GET %s body = %q, want %q", endpoint, got, want)
	}
}

func BenchmarkTakeSnapshot(b *testing.B) {
	dir := b.TempDir()
	for d := range 40 {
		sub := filepath.Join(dir, "pkg"+strconv.Itoa(d))
		if err := os.Mkdir(sub, 0o700); err != nil {
			b.Fatal(err)
		}
		for f := range 50 {
			name := "file" + strconv.Itoa(f) + ".go"
			if f%5 == 0 {
				name = "file" + strconv.Itoa(f) + "_test.go"
			}
			if err := os.WriteFile(filepath.Join(sub, name), nil, 0o600); err != nil {
				b.Fatal(err)
			}
		}
	}
	excludes := Values(filepath.Join(dir, "pkg0"), "*.gen.go")
	var states map[string]fileState
	for b.Loop() {
		states = takeSnapshot(states, Values(dir), excludes)
	}
}
