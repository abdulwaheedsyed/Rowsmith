//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package tunnel

import (
	"errors"
	"net"
	"os"
	"syscall"
)

// socketPair returns the two ends of a kernel socket pair: buffered, with
// deadlines, and with the non-blocking I/O some drivers use.
func socketPair() (net.Conn, net.Conn, error) {
	// Mark both ends close-on-exec before any process can be started, as the
	// net package does where SOCK_CLOEXEC isn't available.
	syscall.ForkLock.RLock()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err == nil {
		syscall.CloseOnExec(fds[0])
		syscall.CloseOnExec(fds[1])
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return nil, nil, err
	}
	fa, fb := os.NewFile(uintptr(fds[0]), "rowsmith-tunnel-a"), os.NewFile(uintptr(fds[1]), "rowsmith-tunnel-b")
	a, errA := net.FileConn(fa)
	b, errB := net.FileConn(fb)
	fa.Close()
	fb.Close()
	if errA != nil || errB != nil {
		if a != nil {
			a.Close()
		}
		if b != nil {
			b.Close()
		}
		return nil, nil, errors.Join(errA, errB)
	}
	return a, b, nil
}
