package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"

	"github.com/fatedier/frp/pkg/util/log"

	"frp-more/internal/manager"
	"frp-more/internal/server"
)

func main() {
	addr := flag.String("addr", ":1332", "HTTP listen address of the management UI")
	dataDir := flag.String("data", "./data", "data directory holding instance configs")
	logLevel := flag.String("log-level", "info", "log level")
	flag.Parse()

	// One global logger for the whole process: all embedded instances share it
	// and it goes to stdout so `docker logs` picks it up.
	// NOTE: frp only writes to stdout when the path is literally "console".
	log.InitLogger("console", *logLevel, 0, false)

	mgr, err := manager.NewManager(*dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "init manager: %v\n", err)
		os.Exit(1)
	}
	mgr.ScanDir()

	srv := server.New(mgr)
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen %s: %v\n", *addr, err)
		os.Exit(1)
	}
	log.Infof("frp-more management UI listening on http://%s", ln.Addr())
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "serve: %v\n", err)
		os.Exit(1)
	}
}
