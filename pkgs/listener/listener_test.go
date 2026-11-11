package listener

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewUnixSocket(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "run", "axum") // parent dir does not exist yet

	ln, err := New("unix:" + path)
	if err != nil {
		t.Fatalf("New(unix:%s): %v", path, err)
	}
	defer func() { _ = ln.Close() }()

	// Parent dir must have been created and the socket must be 0666 so nginx
	// workers can connect.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("%s is not a socket", path)
	}
	if perm := fi.Mode().Perm(); perm != 0o666 {
		t.Fatalf("socket perm = %o, want 666", perm)
	}
}

func TestNewUnixSocketServesHTTP(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s")

	ln, err := New("unix:" + path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
	}
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	resp, err := client.Get("http://localhost/health")
	if err != nil {
		t.Fatalf("GET over unix socket: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestNewUnixSocketReplacesStaleSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s")
	if err := os.WriteFile(path, []byte("not a socket"), 0o644); err != nil {
		t.Fatal(err)
	}
	ln, err := New("unix:" + path)
	if err != nil {
		t.Fatalf("New over stale file: %v", err)
	}
	_ = ln.Close()
}

func TestNewUnixSocketRejectsDirectory(t *testing.T) {
	// /var/run/axum phải là FILE socket. Nếu ai đó tạo nó thành thư mục
	// (mkdir trong entrypoint, emptyDir mount), os.Remove(path) fail với
	// EBUSY/EISDIR — thông báo lỗi phải chỉ thẳng vào nguyên nhân.
	path := filepath.Join(t.TempDir(), "axum")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := New("unix:" + path)
	if err == nil {
		t.Fatal("New over directory: want error, got nil")
	}
	if !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("error = %q, want mention of directory", err)
	}
}

func TestNewTCPUnchanged(t *testing.T) {
	ln, err := New("127.0.0.1:0")
	if err != nil {
		t.Fatalf("New(tcp): %v", err)
	}
	defer func() { _ = ln.Close() }()
	if _, ok := ln.Addr().(*net.TCPAddr); !ok {
		t.Fatalf("addr %s is not TCP", ln.Addr())
	}
}
