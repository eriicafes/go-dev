# Vite integration

Use `github.com/eriicafes/go-dev/vite` when a task reload should refresh
browsers connected to Vite. The Go plugin posts to Vite; a Vite plugin must
receive that request and broadcast the refresh.

## Go configuration

Adapt paths and commands to the existing frontend. This example assumes its
`package.json` is in `web/` and its `dev` script runs Vite.

```go
import (
	"github.com/eriicafes/go-dev"
	"github.com/eriicafes/go-dev/vite"
)

// Inside the development entrypoint:
api := session.NewTask(dev.Cmd{
	Dir:        dev.Dir(".."),
	Run:        dev.Package("./cmd/api"),
	Watch:      dev.Values("cmd", "internal", "templates"),
	ServerAddr: ":8000",
	Commands: dev.Commands(
		dev.Cmd{
			Run:   dev.Binary("pnpm"),
			Args:  dev.Values("dev"),
			Dir:   "web",
			Phase: dev.Before,
		},
	),
	Plugins: dev.Plugins(vite.Refresh{
		Origin: "http://localhost:5173",
		Watch:  dev.Values("templates"),
	}),
})
session.Catch(api.Run())
```

The Go application must listen on its injected `PORT` when using `ServerAddr`.
The Vite child owns its own port and does not need `ServerAddr`.

`Refresh.Origin` defaults to `http://localhost:5173`; match the actual Vite
origin. `Refresh.Watch` filters paths from the Go task's reload events, relative
to that task's `Dir`. It does not create a watcher: include those paths in
`Cmd.Watch`. An empty filter matches all reloads. A non-empty filter does not
match a manual `Reload()` because it has no changed paths. Refresh happens
after replacements, not on the initial process start.

## Vite configuration

Add this plugin to the existing `vite.config.ts` plugins array, preserving the
application's other plugins and settings:

```ts
import type { Plugin } from "vite";

function goDevRefresh(): Plugin {
  return {
    name: "go-dev-refresh",
    configureServer(server) {
      server.middlewares.use("/__go-dev/refresh", (request, response, next) => {
        if (request.method !== "POST") return next();
        server.ws.send({ type: "full-reload" });
        response.statusCode = 204;
        response.end();
      });
    },
  };
}

// Add goDevRefresh() to the existing plugins array.
```

A 404 from the refresh POST means Vite is missing the endpoint. For connection
errors, check the origin and whether Vite is listening. `dev.Before` orders
process startup but does not wait for Vite's HTTP readiness. If Vite falls back
to another port, update `Refresh.Origin` or configure Vite to require a fixed
port.
