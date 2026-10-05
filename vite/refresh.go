// Package vite provides plugins that refresh browsers through a Vite server.
package vite

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/eriicafes/go-dev"
)

const refreshPath = "/__go-dev/refresh"

// Refresh refreshes browsers after matching watched paths cause a task reload.
type Refresh struct {
	// Origin is Vite's HTTP origin. It defaults to http://localhost:5173.
	Origin string

	// Watch lists paths that should refresh the browser after they cause a task
	// reload. Paths are relative to the task's Dir. An empty list matches every
	// reload.
	Watch []string
}

// Use refreshes connected browsers after matching watched paths cause a task
// reload. Vite must expose a POST endpoint at /__go-dev/refresh
// that broadcasts a full-reload message.
func (config Refresh) Use(task *dev.Task) error {
	endpoint, err := config.endpoint()
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 5 * time.Second}
	task.OnReload(func(_ int, paths []string) {
		if !matchesWatch(paths, config.Watch) {
			return
		}
		request, err := http.NewRequest(http.MethodPost, endpoint.String(), nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "dev/vite: create refresh request: %v\n", err)
			return
		}
		response, err := client.Do(request)
		if err != nil {
			fmt.Fprintf(os.Stderr, "dev/vite: refresh browser: %v\n", err)
			return
		}
		_ = response.Body.Close()
		switch {
		case response.StatusCode == http.StatusNotFound:
			// Vite answers 404 when no plugin handles the refresh endpoint.
			fmt.Fprintln(os.Stderr, "dev/vite: missing go-dev refresh plugin in vite config")
		case response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices:
			fmt.Fprintf(os.Stderr, "dev/vite: refresh browser: Vite returned %s\n", response.Status)
		}
	})
	return nil
}

func (config Refresh) endpoint() (*url.URL, error) {
	origin := config.Origin
	if origin == "" {
		origin = "http://localhost:5173"
	}
	endpoint, err := url.Parse(origin)
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" {
		if err == nil {
			err = fmt.Errorf("must include http or https scheme and host")
		}
		return nil, fmt.Errorf("dev/vite: invalid origin %q: %w", origin, err)
	}
	endpoint.Path = strings.TrimSuffix(endpoint.Path, "/") + refreshPath
	endpoint.RawQuery = ""
	return endpoint, nil
}

func matchesWatch(paths, watches []string) bool {
	if len(watches) == 0 {
		return true
	}
	if len(paths) == 0 {
		return false
	}
	for _, path := range paths {
		path = filepath.Clean(path)
		for _, watch := range watches {
			watch = filepath.Clean(watch)
			if path == watch || strings.HasPrefix(path, watch+string(filepath.Separator)) {
				return true
			}
			if matches, _ := filepath.Match(watch, path); matches {
				return true
			}
		}
	}
	return false
}
