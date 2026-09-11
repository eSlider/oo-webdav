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
	"strconv"
	"syscall"

	"github.com/eSlider/oo-webdav/internal/config"
	"github.com/eSlider/oo-webdav/internal/dav"
	"github.com/eSlider/oo-webdav/internal/httpretry"
)

// defaultMaxRetries is how many times a portal refusal is retried before it
// surfaces to the WebDAV client. Override with WEBDAV_MAX_RETRIES.
const defaultMaxRetries = 5

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

	// go-onlyoffice captures http.DefaultClient, so installing a retrying
	// transport here makes every portal API call absorb temporary refusals
	// (429/502/503/504 and rolled-back deadlock 500s) with backoff+jitter
	// instead of failing the WebDAV operation.
	maxRetries := defaultMaxRetries
	if v := os.Getenv("WEBDAV_MAX_RETRIES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			maxRetries = n
		}
	}
	http.DefaultClient = &http.Client{
		Transport: httpretry.New(http.DefaultTransport, maxRetries),
	}

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
