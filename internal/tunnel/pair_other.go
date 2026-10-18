//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly)

package tunnel

import "net"

// socketPair returns an in-memory pipe where there are no Unix socket pairs
// (Windows). It has deadlines too, but no buffer: each write waits for a read.
func socketPair() (net.Conn, net.Conn, error) {
	a, b := net.Pipe()
	return a, b, nil
}
