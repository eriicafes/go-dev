// Package vite provides dev plugins that reload Go apps from Vite HMR events.
package vite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/eriicafes/go-dev"
)

// Config configures the Vite HMR connection used by HMR.
type Config struct {
	// Origin is Vite's HTTP origin. It defaults to http://127.0.0.1:5173. HMR
	// reads Origin/@vite/client to discover Vite's current HMR token.
	Origin string

	// URL is an optional explicit Vite WebSocket URL. Use it when server.ws has
	// a custom host, port, path, or token. When omitted it is derived from
	// Origin and the token in @vite/client.
	URL string

	// ConnectTimeout defaults to 30 seconds and applies to the first HMR
	// connection. Later disconnections reconnect in the background.
	ConnectTimeout time.Duration

	// ReloadDelay coalesces Vite update events before asking the Go session to
	// soft-reload. It defaults to 100ms.
	ReloadDelay time.Duration

	// ReconnectDelay defaults to 1 second.
	ReconnectDelay time.Duration
}

// HMR connects to Vite's HMR WebSocket. Vite "update" and "full-reload"
// messages call Task.Reload.
func HMR(config Config) dev.Plugin {
	return dev.PluginFunc(func(task *dev.Task) error {
		config.setDefaults()
		listener := &listener{
			config:  config,
			task:    task,
			stop:    make(chan struct{}),
			reloads: make(chan struct{}, 1),
			stopped: make(chan struct{}),
		}
		ctx, cancel := context.WithTimeout(context.Background(), config.ConnectTimeout)
		defer cancel()
		connection, err := listener.connectUntil(ctx)
		if err != nil {
			return err
		}
		listener.setConnection(connection)
		go listener.run()
		go listener.reloadLoop()
		task.OnClose(listener.close)
		return nil
	})
}

func (config *Config) setDefaults() {
	if config.Origin == "" {
		config.Origin = "http://127.0.0.1:5173"
	}
	if config.ConnectTimeout <= 0 {
		config.ConnectTimeout = 30 * time.Second
	}
	if config.ReloadDelay <= 0 {
		config.ReloadDelay = 100 * time.Millisecond
	}
	if config.ReconnectDelay <= 0 {
		config.ReconnectDelay = time.Second
	}
}

type listener struct {
	config  Config
	task    *dev.Task
	stop    chan struct{}
	stopped chan struct{}
	reloads chan struct{}

	mu         sync.Mutex
	connection *socket
	closeOnce  sync.Once
}

func (listener *listener) run() {
	defer close(listener.stopped)
	for {
		connection := listener.getConnection()
		if connection == nil {
			ctx, cancel := context.WithCancel(context.Background())
			go func() {
				select {
				case <-listener.stop:
					cancel()
				case <-ctx.Done():
				}
			}()
			var err error
			connection, err = listener.connect(ctx)
			cancel()
			if err != nil {
				if !listener.wait(listener.config.ReconnectDelay) {
					return
				}
				continue
			}
			listener.setConnection(connection)
		}

		payload, err := connection.readText()
		if err != nil {
			connection.close()
			listener.setConnection(nil)
			if !listener.wait(listener.config.ReconnectDelay) {
				return
			}
			continue
		}
		var message struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(payload, &message) == nil && (message.Type == "update" || message.Type == "full-reload") {
			select {
			case listener.reloads <- struct{}{}:
			default:
			}
		}
	}
}

func (listener *listener) connectUntil(ctx context.Context) (*socket, error) {
	var lastErr error
	for {
		connection, err := listener.connect(ctx)
		if err == nil {
			return connection, nil
		}
		lastErr = err
		if !listener.waitContext(ctx, listener.config.ReconnectDelay) {
			return nil, fmt.Errorf("dev/vite: connect before timeout: %w", lastErr)
		}
	}
}

func (listener *listener) reloadLoop() {
	for {
		select {
		case <-listener.stop:
			return
		case <-listener.reloads:
			timer := time.NewTimer(listener.config.ReloadDelay)
			select {
			case <-listener.stop:
				timer.Stop()
				return
			case <-timer.C:
				_ = listener.task.Reload()
			}
		}
	}
}

func (listener *listener) wait(delay time.Duration) bool {
	return listener.waitContext(context.Background(), delay)
}

func (listener *listener) waitContext(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-listener.stop:
		return false
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (listener *listener) close() {
	listener.closeOnce.Do(func() {
		close(listener.stop)
		if connection := listener.getConnection(); connection != nil {
			connection.close()
		}
		<-listener.stopped
	})
}

func (listener *listener) getConnection() *socket {
	listener.mu.Lock()
	defer listener.mu.Unlock()
	return listener.connection
}

func (listener *listener) setConnection(connection *socket) {
	listener.mu.Lock()
	defer listener.mu.Unlock()
	listener.connection = connection
}

func (listener *listener) connect(ctx context.Context) (*socket, error) {
	endpoint, err := listener.endpoint(ctx)
	if err != nil {
		return nil, err
	}
	connection, err := dial(ctx, endpoint)
	if err != nil {
		return nil, fmt.Errorf("dev/vite: connect to Vite HMR at %s: %w", endpoint.Redacted(), err)
	}
	return connection, nil
}

func (listener *listener) endpoint(ctx context.Context) (*url.URL, error) {
	if listener.config.URL != "" {
		endpoint, err := url.Parse(listener.config.URL)
		if err != nil || (endpoint.Scheme != "ws" && endpoint.Scheme != "wss") || endpoint.Host == "" {
			if err == nil {
				err = errors.New("must include ws or wss scheme and host")
			}
			return nil, fmt.Errorf("dev/vite: invalid HMR URL %q: %w", listener.config.URL, err)
		}
		return endpoint, nil
	}

	origin, err := url.Parse(listener.config.Origin)
	if err != nil || (origin.Scheme != "http" && origin.Scheme != "https") || origin.Host == "" {
		if err == nil {
			err = errors.New("must include http or https scheme and host")
		}
		return nil, fmt.Errorf("dev/vite: invalid origin %q: %w", listener.config.Origin, err)
	}
	clientURL := *origin
	clientURL.Path = strings.TrimSuffix(origin.Path, "/") + "/@vite/client"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, clientURL.String(), nil)
	if err != nil {
		return nil, err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("dev/vite: fetch Vite client: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 400 {
		return nil, fmt.Errorf("dev/vite: Vite client returned %s", response.Status)
	}
	client, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	matches := token.FindSubmatch(client)
	if len(matches) != 2 {
		return nil, errors.New("dev/vite: could not find Vite HMR token in @vite/client; set Config.URL explicitly")
	}
	endpoint := &url.URL{Host: origin.Host, Path: strings.TrimSuffix(origin.Path, "/") + "/"}
	if origin.Scheme == "https" {
		endpoint.Scheme = "wss"
	} else {
		endpoint.Scheme = "ws"
	}
	query := endpoint.Query()
	query.Set("token", string(matches[1]))
	endpoint.RawQuery = query.Encode()
	return endpoint, nil
}

var token = regexp.MustCompile(`(?m)const wsToken = ["']([^"']*)["']`)
