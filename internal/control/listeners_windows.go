//go:build windows

package control

import (
	"errors"
	"net"
	"os"
)

// bindControlSocket has no Unix-socket equivalent to bind on Windows. A
// Windows daemon reaches the control plane over the loopback TCP endpoint
// (control.metrics) instead; a configured socket path is a configuration that
// does not apply here, said plainly rather than silently ignored.
func bindControlSocket(socket string) (*os.File, net.Listener, error) {
	return nil, nil, errors.New("control: a Unix control socket is not available on Windows; set control.metrics to a loopback TCP address instead")
}
