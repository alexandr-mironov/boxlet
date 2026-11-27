package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/alexandr-mironov/boxlet/internal/paths"
)

// Повторные служебные запросы к dockerd переиспользуют соединение,
// а не открывают новое на каждый вызов (утечка дескрипторов).
func TestDockerAPIReusesConnection(t *testing.T) {
	dir, err := os.MkdirTemp("", "bxd")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var conns atomic.Int32
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("[]"))
		}),
		ConnState: func(_ net.Conn, s http.ConnState) {
			if s == http.StateNew {
				conns.Add(1)
			}
		},
	}
	go srv.Serve(ln)
	defer srv.Close()

	d := &daemon{layout: paths.Layout{DockerSock: sock}}
	for range 50 {
		if _, err := d.dockerAPI(context.Background(), http.MethodGet, "/containers/json", nil); err != nil {
			t.Fatal(err)
		}
	}
	if n := conns.Load(); n != 1 {
		t.Fatalf("50 запросов открыли %d соединений, want 1", n)
	}
}
