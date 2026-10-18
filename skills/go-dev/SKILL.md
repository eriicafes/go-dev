---
name: go-dev
description: Set up or modify development runners written in Go using github.com/eriicafes/go-dev. Use for arbitrary binaries, optional build steps, process watching and reloading, managed child commands, HTTP readiness and proxies, or Vite browser refresh with this library.
---

# go-dev

Use `github.com/eriicafes/go-dev` to configure development processes in Go. The
package name is `dev`. Building, watching, and proxying are optional and work
with standalone tasks and child commands.

Inspect the application's `go.mod`, existing development entrypoint, server
startup, and frontend scripts before choosing paths, ports, and commands. Keep
the existing layout.

Read [Vite integration](references/vite.md) when adding browser refresh or
running a Vite companion process.

## Development entrypoint

For a new setup, place the entrypoint in a separate package such as
`dev/main.go`. Install the dependency with
`go get github.com/eriicafes/go-dev` if needed. Run it with `go run ./dev`.

```go
package main

import "github.com/eriicafes/go-dev"

func main() {
	session := dev.New()
	session.Catch(session.Run(dev.Cmd{
		Dir:   dev.Dir(".."),
		Run:   dev.Package("."),
		Watch: dev.Values("."),
	}))
	session.Catch(session.Wait())
}
```

- `dev.Package(".")` builds a package into a temporary executable. Use the
  application's actual main package, such as `dev.Package("./cmd/api")`.
- `dev.Binary("pnpm")` starts an existing executable; supply arguments through
  `Args`. A binary name is not a shell command string.
- `dev.Dir("..")` resolves from the Go source file containing the call. Empty
  `Cmd.Dir` uses the shell's working directory. `Watch` paths resolve from `Dir`.
- `session.Run(cmd)` starts a task without returning its handle. Use
  `session.NewTask(cmd)` when you need `URL`, callbacks, middleware, or `Reload`,
  then call `task.Run()` explicitly. Creating a task starts nothing.
- Configure callbacks and `task.Use` before `Run`. Calling `Run` again returns
  the first result; call `Reload` to start a replacement.

## Task orchestration

A session can manage as many tasks as needed. Start multiple tasks with
`session.Run` or `task.Run`, then call `session.Wait()` to keep the development
session running and handle shutdown. Independent tasks can share a session;
use child commands when their lifetime should belong to a particular parent.

Task configuration and orchestration are ordinary Go code. Use functions,
conditions, loops, and synchronization to construct tasks, choose which to
start, and coordinate them. `Run` waits for startup, not process completion.
To finish a one-shot task before starting another, call
`session.Catch(task.Wait())` after starting it.

## Arbitrary binaries and builds

Use `dev.Binary` for any executable, including interpreters, build tools,
workers, and HTTP servers. It has the same watching, replacement, child-command,
and proxy capabilities as `dev.Package`.

```go
worker := session.NewTask(dev.Cmd{
	Dir:   dev.Dir(".."),
	Run:   dev.Binary("node"),
	Args:  dev.Values("worker.mjs", dev.Pair("--log-level", "debug")),
	Env:   dev.OsEnv(dev.Pair("APP_ENV", "development")),
	Watch: dev.Values("worker.mjs"),
})
session.Catch(worker.Run())
```

For a binary that needs building, use a `Prepare` hook to run its build command
before each start and replacement, then point `dev.Binary` at the build output.
Use an absolute path for a project-local executable, such as
`dev.Binary(dev.Dir("..", "bin", "worker"))`. `dev.Package` handles Go package
builds directly. For custom builds with temporary artifacts, implement `Target`
or use `TargetFunc`; return the prepared `*exec.Cmd` and a non-nil cleanup
function on success, and release partial artifacts before returning an error.

## HTTP reloading

`ServerAddr` works with any target, including arbitrary binaries with or without
a build step. Configure it when a stable HTTP proxy and ready replacements are
needed; ordinary process management works without it.

Set `ServerAddr` only when the process can listen on the injected `PORT`
environment variable. The public proxy owns `ServerAddr`; each replacement
listens on its own loopback port. Update the application's development server
startup to read `PORT` if it currently binds a fixed port.

```go
api := session.NewTask(dev.Cmd{
	Dir:             dev.Dir(".."),
	Run:             dev.Package("./cmd/api"),
	Watch:           dev.Values("cmd", "internal", "templates"),
	ServerAddr:      ":8000",
	ServerReadyPath: "/ready",
})
session.Catch(api.Run())
```

Set `ServerReadyPath` only when that endpoint exists and reports readiness with
a 2xx or 3xx status. Leave it empty to check TCP connectivity instead.
`ServerReadyTimeout` defaults to 10 seconds for either check.

With a proxy, the old process serves until a replacement is ready and then
drains for `GracePeriod` (default 5 seconds). A failed build or preparation
leaves the active process running. Without a proxy, the old process stops
after replacement preparation and before the replacement starts. Leave
`ServerAddr` empty for workers and tools that own a fixed port.

## Child commands

Attach companion processes through `Commands: dev.Commands(...)` so they stop
with their parent. Children inherit only the parent's working directory by
default; configure their arguments, environment, and watching as needed. A
relative child `Dir` resolves from the parent's `Dir`.

```go
Commands: dev.Commands(
	dev.Cmd{
		Skip:  os.Getenv("SKIP_ASSETS") == "1",
		Run:   dev.Binary("pnpm"),
		Args:  dev.Values("dev"),
		Dir:   "web",
		Phase: dev.Before,
	},
),
```

Use the frontend's existing package manager and scripts. `dev.Before` starts
the child before parent plugins and the first parent process; `dev.After`
starts it once that process is live. The zero phase starts alongside the
parent. Phases order startup, not completion: use `Prepare` for commands that
must finish before a process starts.

`Skip: true` skips the command and all its children, including preparation and
plugins. `dev.Commands` omits skipped children; direct `[]dev.Cmd` children
are also skipped. Standalone `Run` and `Reload` return nil, and `Wait` returns
nil after the skipped task's `Run` finishes.

## Arguments, environment, and watching

- Use `Args: dev.Values(...)` for explicit arguments or `dev.OsArgs(...)` to
  append arguments passed to the development entrypoint. Forwarding is opt-in.
- Prefer `dev.Pair(key, value)` for environment entries and CLI flags that accept
  `key=value`, such as `dev.Pair("APP_ENV", "development")` and
  `dev.Pair("--log-level", "debug")`. Keep separate arguments when the CLI
  requires them.
- Nil `Env` inherits the process environment. Any non-nil `Env` replaces it;
  use `dev.OsEnv(dev.Pair("APP_ENV", "development"))` for inherited variables
  plus overrides, or `dev.Getenv("PATH", "HOME")` for selected inherited
  variables.
- `BuildEnv` adds variables to the inherited environment for package builds.
  Use `dev.Values(dev.Pair("CGO_ENABLED", "0"))`, for example. It does not
  configure the application's runtime environment.
- Watch the source, templates, and assets that require a process replacement.
  Exclude generated outputs when watching them would cause a rebuild loop.
  `.git`, `node_modules`, and `*_test.go` files are ignored by default.
- `WatchExclude` accepts paths and `filepath.Match` patterns. A pattern without
  a path separator matches names at any depth. Watching polls every 250ms by
  default; `ReloadDelay` coalesces changes for 100ms.

## Preparation

Use `Prepare: dev.Hooks(dev.HookFunc(...))` for synchronous generation or
validation before every initial process and replacement. Hook functions have
signature `func(context.Context, dev.Cmd) error`; use the supplied context and
resolved `cmd.Dir` when running subprocesses. Return errors to abort the start
attempt. Use `exec.CommandContext` when a hook needs a subprocess.

## Plugins

Use plugins to package shared behavior for multiple tasks, such as lifecycle
logging, proxy middleware, or browser refresh. Apply them through
`Plugins: dev.Plugins(...)`. A plugin implements `Use(*dev.Task) error` and
configures each task once before its first process. Use `dev.PluginFunc` to
adapt a function into a plugin. Return setup errors to the caller.

For behavior specific to one task, callbacks and middleware can also be
configured directly on its handle before `Run`.
`OnStart`, `OnReload`, and `OnProcessExit` callbacks run asynchronously; protect
shared state. Reload paths are relative to `Cmd.Dir`; manual reloads supply no
paths. Register plugin cleanup with `OnClose`.

## Shutdown and error handling

`session.Wait()` waits for an interrupt or termination signal and closes all
managed tasks. `task.Wait()` waits for that task and returns the process exit
error for an unwatched process; watched tasks stay alive until closed. Use
`task.Wait()` for a one-shot command and close the session afterward.
`Session.Close(ctx)` and `Task.Close(ctx)` are safe to repeat.

Prefer `session.Catch(...)` to handle errors from starting, reloading, waiting
for, or closing tasks and sessions. For a non-nil error, it closes the session,
prints the error and any shutdown error to stderr, and exits with status 1.
A nil error does nothing. Return errors from preparation hooks and plugin
setup so the entrypoint can handle them through `session.Catch(task.Run())`.
The session shutdown timeout defaults to 10 seconds through `WaitTimeout`.

## Verifying entrypoint changes

To validate edits to the development configuration, format the changed Go files
and compile the entrypoint, for example with
`go build -o /tmp/go-dev-entrypoint ./dev`. This is a code check; it does not
start development processes. Use `go run ./dev` to run the development session.

When testing runtime behavior, start the session, check a watched change and
any configured readiness checks, then stop the session so its managed
processes are closed.
