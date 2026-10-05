# go-dev

Build, run, and reload Go applications from a Go development entrypoint.

## Install

```sh
go get github.com/eriicafes/go-dev
```

Create a development entrypoint such as `dev/main.go`, then run it with:

```sh
go run ./dev
```

It does not need to be part of the application package.

## Quick start

```go
package main

import (
	"log"

	"github.com/eriicafes/go-dev"
)

func main() {
	session := dev.New()
	task, err := session.RunTask(dev.Cmd{
		Dir:   dev.Dir(".."),
		Run:   dev.Package("."),
		Watch: dev.Values("."),

		// Set this to expose the application through a stable HTTP proxy.
		ServerAddr: ":8000",

		// Set this when the application exposes a readiness endpoint.
		ServerHealthPath: "/healthz",
	})
	session.Catch(err)

	log.Printf("development proxy listening on %s", task.URL())
	session.Catch(session.Wait())
}
```

```text
.
├── dev/
│   └── main.go
├── go.mod
└── main.go
```

`dev.Package` builds its package into a temporary executable. `dev.Binary` starts
an existing executable. Each replacement behind a proxy receives its own
loopback port through the `PORT` environment variable.

## Reloading

When a watched path changes, go-dev builds a replacement process. With
`ServerAddr` set, it exposes a stable HTTP proxy:

1. The replacement starts on a private loopback port.
2. When `ServerHealthPath` returns a 2xx or 3xx response, new proxy requests
   switch to it.
3. The old process may continue handling in-flight work for `GracePeriod`.

`ServerHealthTimeout` defaults to 30 seconds when a health path is set.
Leaving `ServerHealthPath` empty promotes the replacement as soon as it starts.

## Cmd configuration

### Paths

`Dir` is the working directory for processes and package builds, and the base for
relative `Watch` paths. An empty `Dir` uses the current working directory.

`dev.Dir` resolves paths from the Go source file containing the call, which is
especially useful from a separate `dev` package:

```go
Dir: dev.Dir(".."),
```

`dev.Dir()` returns that source file's directory. `dev.Dir(".")` resolves the
directory containing the development configuration.

### Arguments and environment

`Args` are passed to every process. Arguments supplied to the development
entrypoint are forwarded as well. `Env` adds environment variables to every
process. `BuildEnv` adds environment variables to `dev.Package` builds.

```go
err := session.Run(dev.Cmd{
	Run: dev.Package("."),
	Args: dev.Values(
		"-log-level=debug",
		dev.Pair("-log-format", "json"),
	),
	Env: dev.Values(
		"APP_ENV=development",
		dev.Pair("LOG_LEVEL", "debug"),
	),
	BuildEnv: dev.Values(
		"CGO_ENABLED=1",
	),
})
session.Catch(err)
```

Both processes start with the operating-system environment. `dev.Values` makes
slices concise. `dev.Pair` joins two values with an equals sign.

### Watching and timing

`Watch` lists files or directories to poll. Changes to `*_test.go` files and
paths named `.git` or `node_modules` are ignored by default. `WatchExclude`
omits additional files or directories and supports `filepath.Match` patterns.
The default polling interval is 250ms. A detected change waits for
`ReloadDelay`, which defaults to 100ms, so nearby edits are coalesced.
`GracePeriod` defaults to 5 seconds.

## Commands

`Commands` creates child tasks for related long-running processes. A child
inherits its parent's working directory unless it sets `Dir`; a relative child
directory resolves from the parent's `Dir`. A child stops when its parent task
or the development session closes.

```go
err := session.Run(dev.Cmd{
	Run: dev.Package("."),
	Commands: dev.Commands(
		dev.Cmd{
			Run:   dev.Binary("pnpm"),
			Args:  dev.Values("dev"),
			Phase: dev.Before,
		},
		dev.Cmd{
			Run:   dev.Binary("go"),
			Args:  dev.Values("run", "./cmd/worker"),
			Phase: dev.After,
		},
	),
})
session.Catch(err)
```

`dev.Before` starts a command before plugins and the parent's first process.
`dev.After` starts it after the first process is live. The zero phase starts
alongside the parent without an ordering guarantee.

You can also run a command as its own task. Its `Dir` is resolved from the
current working directory, and `Watch` makes it restart on changes. The task's
`Reload` method restarts it explicitly.

```go
assets, err := session.RunTask(dev.Cmd{
	Run:   dev.Binary("pnpm"),
	Args:  dev.Values("dev"),
	Watch: dev.Values("./web"),
})
session.Catch(err)

// _ = assets.Reload()
```

Use `session.Run` when you do not need the returned task.

### HTTP processes

A `Cmd` that reads `PORT` can run behind a stable proxy and receive soft
replacement behavior:

```go
api, err := session.RunTask(dev.Cmd{
	Run:                 dev.Binary("node"),
	Args:                dev.Values("server.mjs"),
	ServerAddr:          ":8001",
	ServerHealthPath:    "/health",
})
session.Catch(err)

log.Printf("API listening on %s", api.URL())
```

A command that owns a fixed port, such as an ordinary Vite server, can omit
`ServerAddr` and remains managed without a proxy.

## Plugins

Plugins configure a task before its first process starts. Use
`dev.PluginFunc` for a small inline plugin or provide a type that implements
`Use(*dev.Task) error`.

```go
Plugins: dev.Plugins(
	dev.PluginFunc(func(task *dev.Task) error {
		onLive := func(pid int) {
			log.Printf("process %d is live", pid)
		}
		task.OnStart(onLive)
		task.OnReload(func(pid int) {
			onLive(pid)
		})
		return nil
	}),
),
```

### Vite HMR

`github.com/eriicafes/go-dev/vite` listens to an already running Vite
development server. Vite `update` and `full-reload` messages call
`Task.Reload`, so the Go application is replaced when frontend changes arrive.

```go
import "github.com/eriicafes/go-dev/vite"

api, err := session.RunTask(dev.Cmd{
	Run:              dev.Package("."),
	Watch:            dev.Values("."),
	ServerAddr:       ":8000",
	ServerHealthPath: "/health",
	Commands: dev.Commands(
		dev.Cmd{
			Run:   dev.Binary("pnpm"),
			Args:  dev.Values("vite"),
			Phase: dev.Before,
		},
	),
	Plugins: dev.Plugins(
		vite.HMR(vite.Config{Origin: "http://127.0.0.1:5173"}),
	),
})
session.Catch(err)

log.Printf("API listening on %s", api.URL())
```

`Origin` defaults to `http://127.0.0.1:5173`. Set `vite.Config.URL` to a full
`ws://` or `wss://` URL when Vite uses a custom WebSocket host, port, path, or
token.

## Lifecycle and shutdown

### Session

#### `Session.Wait`

Wait blocks until the session receives an interrupt or termination signal, then
closes every managed task. `Session.WaitTimeout` controls the graceful-shutdown
limit and defaults to 10 seconds.

#### `Session.Close`

Close shuts down every managed task. It is safe to call more than once.

#### `Session.Done`

Done closes after the full development session shuts down.

#### `Session.Catch`

Catch closes the development session and panics with a non-nil error. It keeps a
small `main` function straightforward:

### Task

#### `Task.Reload`

Reload starts a replacement process. When `ServerAddr` is configured, the old
process remains available until the replacement becomes live.

#### `Task.OnStart`

OnStart runs asynchronously after the task's first process becomes live. It
receives that process's PID.

#### `Task.OnReload`

OnReload runs asynchronously after each replacement process becomes live. It
receives that process's PID.

#### `Task.OnProcessExit`

OnProcessExit runs asynchronously whenever a managed process exits. It receives
the process's PID and exit error.

#### `Task.Wait`

Wait blocks until the task finishes. When an unwatched task's active process
ends before the task is closed, Wait returns that process's exit error. A watched
task continues waiting for changes until it is closed.

#### `Task.Done`

Done closes when an unwatched process exits, or after a watched task finishes
closing.

#### `Task.OnClose`

OnClose registers cleanup that runs when the task closes.

#### `Task.Close`

Close stops the task and its child tasks. It is safe to call more than once.
