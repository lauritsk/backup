package engine

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/lauritsk/backup/internal/appbackup/config"
)

func TestCheckUsesEngineUnixSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix sockets are not available")
	}
	directory, err := os.MkdirTemp("/tmp", "appbackup-engine-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(directory)
	socket := filepath.Join(directory, "engine.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var mutex sync.Mutex
	seen := map[string]int{}
	server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mutex.Lock()
		seen[request.URL.Path]++
		mutex.Unlock()
		switch request.URL.Path {
		case "/_ping":
			_, _ = writer.Write([]byte("OK"))
		case "/version":
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"Version":"1.0"}`))
		default:
			http.NotFound(writer, request)
		}
	})}
	go server.Serve(listener)
	defer server.Close()

	for _, engineType := range []string{"docker", "podman"} {
		if err := (Runner{}).Check(context.Background(), config.EngineConfig{Type: engineType, Socket: socket}); err != nil {
			t.Fatalf("Check(%s): %v", engineType, err)
		}
	}
	mutex.Lock()
	defer mutex.Unlock()
	if seen["/_ping"] != 2 || seen["/version"] != 2 {
		t.Fatalf("requests = %#v", seen)
	}
}
