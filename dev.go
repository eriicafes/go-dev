// Package dev builds, runs, and reloads Go applications or CLI commands from Go configuration.
package dev

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
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

// OsArgs returns args followed by the arguments supplied to the development
// entrypoint, without the program name. Use it for Cmd.Args to forward them to
// a process.
func OsArgs(args ...string) []string { return slices.Concat(args, os.Args[1:]) }

// OsEnv returns the operating-system environment followed by pairs. Use it for
// Cmd.Env to add or override variables while inheriting the rest.
func OsEnv(pairs ...string) []string { return slices.Concat(os.Environ(), pairs) }

// Getenv returns KEY=value pairs for the keys that are set in the
// operating-system environment. Use it for Cmd.Env to inherit only those keys.
func Getenv(keys ...string) []string {
	pairs := make([]string, 0, len(keys))
	for _, key := range keys {
		if value, ok := os.LookupEnv(key); ok {
			pairs = append(pairs, Pair(key, value))
		}
	}
	return pairs
}
