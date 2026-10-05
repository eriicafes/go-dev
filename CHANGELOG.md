# go-dev

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
