package vite_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/eriicafes/go-dev"
	devvite "github.com/eriicafes/go-dev/vite"
)

func TestHMRReloadsForViteUpdates(t *testing.T) {
	vite := newFakeVite(t)
	t.Cleanup(vite.close)
	file := filepath.Join(t.TempDir(), "app.go")
	writeGoApp(t, file, "ok")
	live := make(chan int, 2)
	server := dev.New()
	task, err := server.RunTask(dev.Cmd{
		GracePeriod:      10 * time.Millisecond,
		Run:              dev.Package(file),
		ServerAddr:       "127.0.0.1:0",
		ServerHealthPath: "/",
		Plugins: dev.Plugins(
			devvite.HMR(devvite.Config{Origin: vite.server.URL, ConnectTimeout: time.Second}),
			dev.PluginFunc(func(task *dev.Task) error {
				onLive := func(pid int) { live <- pid }
				task.OnStart(onLive)
				task.OnReload(onLive)
				return nil
			}),
		),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close(context.Background()) })
	assertBody(t, task.URL(), "ok")
	firstPID := <-live
	if firstPID <= 0 {
		t.Fatalf("first live pid = %d, want positive", firstPID)
	}
	<-vite.connected
	vite.updates <- struct{}{}
	select {
	case got := <-live:
		if got <= 0 || got == firstPID {
			t.Fatalf("Vite-triggered pid = %d, want a new positive pid", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Vite update did not trigger a reload")
	}
}

type fakeVite struct {
	server    *httptest.Server
	connected chan struct{}
	updates   chan struct{}
	once      sync.Once
}

func newFakeVite(t *testing.T) *fakeVite {
	t.Helper()
	vite := &fakeVite{connected: make(chan struct{}), updates: make(chan struct{}, 1)}
	vite.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/@vite/client" {
			_, _ = io.WriteString(writer, `const wsToken = "test-token"`)
			return
		}
		if request.Header.Get("Upgrade") != "websocket" || request.Header.Get("Sec-WebSocket-Protocol") != "vite-hmr" || request.URL.Query().Get("token") != "test-token" {
			http.Error(writer, "bad Vite HMR connection", http.StatusBadRequest)
			return
		}
		hijacker := writer.(http.Hijacker)
		connection, buffer, err := hijacker.Hijack()
		if err != nil {
			return
		}
		defer connection.Close()
		_, _ = fmt.Fprint(buffer, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Protocol: vite-hmr\r\n\r\n")
		_ = buffer.Flush()
		vite.once.Do(func() { close(vite.connected) })
		<-vite.updates
		writeWebSocketText(buffer, []byte(`{"type":"update"}`))
		_, _ = io.Copy(io.Discard, buffer)
	}))
	return vite
}

func (vite *fakeVite) close() {
	select {
	case vite.updates <- struct{}{}:
	default:
	}
	vite.server.Close()
}

func writeWebSocketText(buffer *bufio.ReadWriter, payload []byte) {
	_, _ = buffer.Write([]byte{0x81, byte(len(payload))})
	_, _ = buffer.Write(payload)
	_ = buffer.Flush()
}

func writeGoApp(t *testing.T, file, message string) {
	t.Helper()
	source := `package main
import (
    "fmt"
    "net/http"
    "os"
)
func main() {
    _ = http.ListenAndServe("127.0.0.1:" + os.Getenv("PORT"), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
        _, _ = fmt.Fprint(w, ` + strconv.Quote(message) + `)
    }))
}`
	if err := os.WriteFile(file, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertBody(t *testing.T, endpoint, want string) {
	t.Helper()
	response, err := http.Get(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(body); got != want {
		t.Fatalf("GET %s body = %q, want %q", endpoint, got, want)
	}
}
