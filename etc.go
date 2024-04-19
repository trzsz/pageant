//go:build !windows
// +build !windows

package pageant

import (
	"fmt"
	"net"
	"os"
)

// NewConn creates a new connection to Pageant or agent.
// Ensure Close gets called on the returned Conn when it is no longer needed.
func NewConn() (net.Conn, error) {
	const sshAuthSock = "SSH_AUTH_SOCK"
	socket := os.Getenv(sshAuthSock)
	if socket == "" {
		return nil, fmt.Errorf("empty %s", sshAuthSock)
	}
	return net.Dial("unix", socket)
}

// used in establishConn
func PageantWindow() (window uintptr, err error) {
	return 0, fmt.Errorf("cannot find Pageant window, ensure Pageant is running and runtime.GOOS==`windows`")
}
