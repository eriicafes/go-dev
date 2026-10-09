package dev

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Target prepares a process for a Cmd. On success, Cmd must return a non-nil
// cleanup function that releases any resources created while preparing the
// process.
type Target interface {
	Cmd(Cmd) (*exec.Cmd, func(), error)
}

// TargetFunc adapts a function into a Target.
type TargetFunc func(Cmd) (*exec.Cmd, func(), error)

// Cmd prepares a command with target.
func (target TargetFunc) Cmd(cmd Cmd) (*exec.Cmd, func(), error) {
	if target == nil {
		return nil, nil, errors.New("dev: target function is required")
	}
	return target(cmd)
}

// Binary starts name directly.
type Binary string

// Cmd prepares a command that runs name.
func (name Binary) Cmd(Cmd) (*exec.Cmd, func(), error) {
	if name == "" {
		return nil, nil, errors.New("dev: binary name is required")
	}
	return exec.Command(string(name)), func() {}, nil
}

// Package builds and starts a Go package.
type Package string

// Cmd builds path and prepares its binary to run.
func (path Package) Cmd(config Cmd) (*exec.Cmd, func(), error) {
	dir, err := os.MkdirTemp("", "go-dev-")
	if err != nil {
		return nil, nil, fmt.Errorf("dev: create process directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	binary := filepath.Join(dir, "app")
	packagePath := string(path)
	if packagePath == "" {
		packagePath = "."
	}
	build := exec.Command("go", "build", "-o", binary, packagePath)
	build.Dir = config.Dir
	build.Env = append(os.Environ(), config.BuildEnv...)
	build.Stdout = os.Stdout
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("dev: build process: %w", err)
	}
	return exec.Command(binary), cleanup, nil
}

// Phase controls when a child Cmd starts relative to its parent. The zero
// value starts alongside the parent without an ordering guarantee.
type Phase int

const (
	// Before starts a command before plugins and the parent's first process.
	Before Phase = -1

	// After starts a command after the parent's first process is live.
	After Phase = 1
)

// Cmd configures a managed process.
type Cmd struct {
	// Run selects the process to start. Use Binary to start an executable or
	// Package to build and start a Go package.
	Run Target

	// Args supplies command-line arguments to every process.
	Args []string
	// Env is the environment of every process. When nil, processes inherit the
	// operating-system environment. Use OsEnv to inherit it with additions.
	Env []string
	// BuildEnv adds environment variables to the inherited environment of a
	// Package build.
	BuildEnv []string
	// Dir is the working directory. For child commands, relative paths resolve
	// from the parent command's Dir; otherwise they resolve from the current
	// working directory.
	Dir string
	// Watch reloads the command when one of its paths changes. Relative paths
	// resolve from Dir.
	Watch []string
	// WatchExclude omits files, directories, or filepath.Match patterns from
	// Watch. Relative paths resolve from Dir.
	WatchExclude []string

	// ServerAddr enables a stable HTTP proxy. When empty, the command runs
	// without a proxy or readiness checks.
	ServerAddr string
	// ServerReadyPath enables HTTP readiness checks when ServerAddr is
	// non-empty. A 2xx or 3xx response marks a process live. The zero value
	// marks a process live once its port accepts connections.
	ServerReadyPath string
	// ServerReadyTimeout bounds one process's readiness check. It defaults to
	// 10 seconds when ServerAddr is set.
	ServerReadyTimeout time.Duration

	// PollInterval is how often Watch paths are checked. It defaults to 250ms.
	PollInterval time.Duration
	// ReloadDelay waits after a detected change before reloading. It defaults to
	// 100ms and coalesces nearby file changes.
	ReloadDelay time.Duration
	// GracePeriod is how long a previous live process can drain. It defaults
	// to 5 seconds.
	GracePeriod time.Duration

	// Commands starts child tasks with this command.
	Commands []Cmd
	// Plugins configure the task before its first process starts.
	Plugins []Plugin

	// Phase controls when this Cmd starts when it belongs to Commands.
	Phase Phase
}

// Commands returns a slice of commands.
func Commands(commands ...Cmd) []Cmd { return commands }

// Plugin configures a task before its first process starts.
type Plugin interface {
	Use(*Task) error
}

// Plugins returns a slice of plugins.
func Plugins(plugins ...Plugin) []Plugin { return plugins }

// PluginFunc adapts a function into a Plugin.
type PluginFunc func(task *Task) error

func (plugin PluginFunc) Use(task *Task) error { return plugin(task) }
