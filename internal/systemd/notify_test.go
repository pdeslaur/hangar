package systemd

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestReady(t *testing.T) {
	// t.TempDir's paths can exceed a socket path's 104 bytes on macOS.
	dir, err := os.MkdirTemp("", "notify")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "sock")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	t.Setenv("NOTIFY_SOCKET", path)

	if err := Ready(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(buf[:n]); got != "READY=1" {
		t.Errorf("got %q, want READY=1", got)
	}
}

func TestReadyWithoutSocket(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	if err := Ready(); err != nil {
		t.Errorf("without NOTIFY_SOCKET: %v", err)
	}
}
