// Package config loads the ooshare sidecar configuration from environment.
package config

import (
	"os"
	"strings"
	"time"
)

// Config holds all runtime configuration for the WebDAV sidecar.
type Config struct {
	// PortalURL is the base URL of the ONLYOFFICE portal API, e.g.
	// "http://onlyoffice:80". No trailing slash.
	PortalURL string
	// ListenAddr is the address the HTTP WebDAV server binds to, e.g. ":8088".
	ListenAddr string
	// WebDAVPrefix is the URL path prefix under which WebDAV is served.
	// Requests at /webdav/... are mapped to the file tree.
	WebDAVPrefix string
	// Realm is the HTTP Basic auth realm string.
	Realm string
	// WebDAVRoot is the portal folder id mapped to the WebDAV root "/".
	// Defaults to "@root", the aggregated view with the virtual sections
	// ("In projects", "Shared with me", "My documents", "Common", "Favorites",
	// "Recent", "Trash"). Use "@my" for just the user's My Documents.
	WebDAVRoot string
	// CacheTTL is how long folder listings are kept in the per-user cache.
	CacheTTL time.Duration
	// SessionTTL is how long a per-user authenticated session is kept alive
	// before re-validating credentials against the portal.
	SessionTTL time.Duration
}

// Load reads configuration from environment variables, applying defaults.
func Load() Config {
	return Config{
		PortalURL:    strings.TrimRight(env("PORTAL_URL", "http://onlyoffice:80"), "/"),
		ListenAddr:   env("LISTEN_ADDR", ":8088"),
		WebDAVPrefix: env("WEBDAV_PREFIX", "/webdav"),
		Realm:        env("WEBDAV_REALM", "ONLYOFFICE WebDAV"),
		WebDAVRoot:   env("WEBDAV_ROOT_ID", "@root"),
		CacheTTL:     envDur("CACHE_TTL", 10*time.Second),
		SessionTTL:   envDur("SESSION_TTL", 15*time.Minute),
	}
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envDur(key string, def time.Duration) time.Duration {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
