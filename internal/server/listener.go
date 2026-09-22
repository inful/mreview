package server

import (
	"context"
	"net"
)

// newListener is the function used to create the TCP listener.
// Exposed as a variable so tests can stub it (e.g. to capture
// the bound port).
var newListener = func(_ context.Context, addr string) (net.Listener, error) {
	return net.Listen("tcp", addr)
}
