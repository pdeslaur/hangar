// Package systemd tells systemd when the service is ready, through the
// notification socket podman passes to containers whose quadlet sets
// Notify=true.
package systemd

import (
	"net"
	"os"
	"strings"
)

// Ready sends READY=1 to the socket in NOTIFY_SOCKET, if it's set, so that
// units ordered after this one start, and a unit's ExecStartPost= runs. A
// socket starting with "@" is in the abstract namespace.
func Ready() error {
	path := os.Getenv("NOTIFY_SOCKET")
	if path == "" {
		return nil
	}
	if name, ok := strings.CutPrefix(path, "@"); ok {
		path = "\x00" + name
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write([]byte("READY=1"))
	return err
}
