// Package dev builds, runs, and reloads Go applications or CLI commands from Go configuration.
package dev

import (
	"path/filepath"
	"runtime"
)

// Dir joins paths relative to the Go source file that calls it. With no paths,
// it returns that file's directory.
// Use it for Cmd.Dir to keep a development entrypoint independent
// of the shell's working directory.
func Dir(paths ...string) string {
	_, file, _, ok := runtime.Caller(1)
	if !ok {
		return filepath.Join(paths...)
	}
	return filepath.Clean(filepath.Join(append([]string{filepath.Dir(file)}, paths...)...))
}

// Values returns a slice containing values.
func Values[T any](values ...T) []T { return values }

// Pair joins key and value with an equals sign.
func Pair(key, value string) string { return key + "=" + value }
