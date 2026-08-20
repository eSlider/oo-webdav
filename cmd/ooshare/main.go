// Command ooshare runs the ONLYOFFICE WebDAV sidecar. It replaces the legacy
// Node ASC.WebDav service, exposing the portal's Documents over WebDAV with
// HTTP Basic authentication against portal users.
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/eSlider/oo-webdav/internal/config"
	"github.com/eSlider/oo-webdav/internal/dav"
)

// Build-time variables, set via -ldflags in the release pipeline.
var (
	version = "dev"
	commit  = ""
	date    = ""
)

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		log.Printf("oo-webdav %s (commit %s, built %s)", version, commit, date)
		return
	}

	cfg := config.Load()

	srv := dav.New(cfg)
	httpServer := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: srv.Handler(),
	}

	go func() {
		log.Printf("ooshare webdav listening on %s (prefix %s) -> %s",
			cfg.ListenAddr, cfg.WebDAVPrefix, cfg.PortalURL)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Println("shutting down")
}
