package vite

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/eriicafes/go-dev"
)

func TestMatchesWatch(t *testing.T) {
	tests := []struct {
		name    string
		paths   []string
		watches []string
		want    bool
	}{
		{name: "directory", paths: []string{"templates/index.html"}, watches: []string{"templates"}, want: true},
		{name: "pattern", paths: []string{"templates/index.html"}, watches: []string{"templates/*.html"}, want: true},
		{name: "unmatched", paths: []string{"routes/index.go"}, watches: []string{"templates"}},
		{name: "empty watches", paths: []string{"routes/index.go"}, want: true},
		{name: "empty watches without paths", want: true},
		{name: "no paths", watches: []string{"templates"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := matchesWatch(test.paths, test.watches); got != test.want {
				t.Fatalf("matchesWatch(%q, %q) = %t, want %t", test.paths, test.watches, got, test.want)
			}
		})
	}
}

func TestRefreshPostsAfterMatchingReload(t *testing.T) {
	refreshed := make(chan struct{}, 1)
	vite := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != refreshPath {
			http.NotFound(writer, request)
			return
		}
		refreshed <- struct{}{}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer vite.Close()
	dir := t.TempDir()
	file := filepath.Join(dir, "app.go")
	if err := os.WriteFile(file, []byte(`package main
import (
    "net/http"
    "os"
)
func main() {
    _ = http.ListenAndServe("127.0.0.1:" + os.Getenv("PORT"), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
        _, _ = w.Write([]byte("ok"))
    }))
}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	session := dev.New()
	task := session.NewTask(dev.Cmd{
		Dir:             dir,
		Run:             dev.Package(file),
		Watch:           dev.Values("."),
		PollInterval:    10 * time.Millisecond,
		ReloadDelay:     5 * time.Millisecond,
		ServerAddr:      "127.0.0.1:0",
		ServerReadyPath: "/",
		Plugins:         dev.Plugins(Refresh{Origin: vite.URL}),
	})
	if err := task.Run(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close(context.Background()) }()
	time.Sleep(30 * time.Millisecond)
	if err := os.Mkdir(filepath.Join(dir, "templates"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "templates", "index.html"), []byte("updated"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-refreshed:
	case <-time.After(10 * time.Second):
		t.Fatal("matching watched path did not refresh Vite")
	}
	if task.URL() == "" {
		t.Fatal("task URL = empty")
	}
}
