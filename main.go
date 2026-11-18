// The forge entry point: bind an ephemeral port, print it, serve until the
// process is stopped. The launcher (main.ts) reads the port from stdout and
// registers it with the environment coordinator.

package main

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

func main() {
	store := newStore()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[entry] listen: %v\n", err)
		os.Exit(1)
	}
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		fmt.Fprintln(os.Stderr, "[entry] expected a TCP address after opening the listener")
		os.Exit(1)
	}
	fmt.Printf("[entry] listening on port %d\n", addr.Port)

	// Shut down cleanly on SIGTERM / SIGINT so the launcher can stop us.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-stop
		listener.Close()
	}()

	// Raw HTTP/1.1 transport (rawhttp.go): same handlers as the net/http
	// fallback, without its per-request overhead.
	if err := serveRaw(store, listener); err != nil && !strings.Contains(err.Error(), "use of closed network connection") {
		fmt.Fprintf(os.Stderr, "[entry] serve: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "[entry] done")
}
