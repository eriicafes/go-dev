# go-dev

## 0.3.0

### Minor Changes

- 503f852: Add deferred task startup with Session.NewTask and Task.Run.
- 957ac36: Add Prepare hooks that can validate or prepare a command before it
  starts or reloads.
- 503f852: Add TargetFunc for custom process targets.

## 0.2.0

### Minor Changes

- 702ac53: Add watch exclusions with filepath.Match patterns and ignore Go test files by
  default.
- 2607f2c: Add dev.OsArgs and dev.OsEnv. Entrypoint arguments are no longer forwarded and a
  non-nil Env replaces the inherited environment.
- c821c11: Add distinct task hooks for the first live process, later live replacements,
  and managed process exits.
- 27322ab: Rename ServerHealthPath and ServerHealthTimeout to ServerReadyPath and
  ServerReadyTimeout.
- d408441: Add Task.Wait and expose Target for custom process preparation.
- 10336b5: Add a Vite refresh plugin that reloads connected browsers after matching task
  reloads.
