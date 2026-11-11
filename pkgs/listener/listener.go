// Package listener builds net.Listeners from the ADDR env convention:
// "unix:/path/to/socket" for nginx-facing unix sockets (plans/34), anything
// else (":8080", "127.0.0.1:8080") is a plain TCP address.
package listener

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
)

// New returns a listener for addr. For "unix:<path>" it creates the parent
// directory, removes a stale socket left by a previous unclean shutdown and
// chmods the socket 0666 so nginx workers (non-root) can connect.
func New(addr string) (net.Listener, error) {
	if path, ok := unixPath(addr); ok {
		if dir := filepath.Dir(path); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("listener: mkdir %s: %w", dir, err)
			}
		}
		// unix sockets persist on disk; a stale one makes bind fail with EADDRINUSE.
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("listener: remove stale socket %s: %w", path, err)
		}
		ln, err := net.Listen("unix", path)
		if err != nil {
			return nil, fmt.Errorf("listener: unix %s: %w", path, err)
		}
		if err := os.Chmod(path, 0o666); err != nil {
			_ = ln.Close()
			return nil, fmt.Errorf("listener: chmod %s: %w", path, err)
		}
		return ln, nil
	}
	return net.Listen("tcp", addr)
}

func unixPath(addr string) (string, bool) {
	const prefix = "unix:"
	if len(addr) > len(prefix) && addr[:len(prefix)] == prefix {
		return addr[len(prefix):], true
	}
	return "", false
}
