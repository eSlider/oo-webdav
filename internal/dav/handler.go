// Package dav wires the ONLYOFFICE-backed filesystem to a WebDAV HTTP server
// with HTTP Basic authentication against portal users.
package dav

import (
	"context"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/eslider/go-onlyoffice"
	"golang.org/x/net/webdav"

	"github.com/eSlider/oo-webdav/internal/config"
)

// Server authenticates portal users and serves WebDAV against the per-user
// filesystem.
type Server struct {
	cfg   config.Config
	mu    sync.Mutex
	sess  map[string]*session
	lock  webdav.LockSystem
}

// session is a per-user authenticated handle.
type session struct {
	user       string
	fs         *fs
	handler    *webdav.Handler
	lastActive time.Time
}

// New builds a Server from configuration.
func New(cfg config.Config) *Server {
	return &Server{
		cfg:  cfg,
		sess: make(map[string]*session),
		lock: webdav.NewMemLS(),
	}
}

// Handler returns the root http.Handler for the server. /healthz is public;
// everything else is authenticated and served by the per-user WebDAV handler.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("ok\n"))
			return
		}
		s.auth(nil).ServeHTTP(w, r)
	})
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user == "" {
			w.Header().Set("WWW-Authenticate", `Basic realm="`+s.cfg.Realm+`"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		sess, err := s.getSession(r.Context(), user, pass)
		if err != nil {
			// Any authentication error is surfaced as 401 (wrong credentials);
			// the portal rejects unknown users / bad passwords with a 500.
			log.Printf("session %q: %v", user, err)
			w.Header().Set("WWW-Authenticate", `Basic realm="`+s.cfg.Realm+`"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		sess.handler.ServeHTTP(w, r)
	})
}

func (s *Server) getSession(ctx context.Context, user, pass string) (*session, error) {
	s.mu.Lock()
	if se, ok := s.sess[user]; ok && time.Since(se.lastActive) < s.cfg.SessionTTL {
		se.lastActive = time.Now()
		s.mu.Unlock()
		return se, nil
	}
	s.mu.Unlock()

	client := onlyoffice.NewClient(onlyoffice.Credentials{
		Url:      s.cfg.PortalURL,
		User:     user,
		Password: pass,
	})
	if err := client.AuthenticateContext(ctx); err != nil {
		return nil, err
	}

	se := &session{
		user:       user,
		fs:         newFS(client, s.cfg.WebDAVRoot, s.cfg.CacheTTL),
		lastActive: time.Now(),
	}
	se.handler = &webdav.Handler{
		Prefix:     s.cfg.WebDAVPrefix,
		FileSystem: se.fs,
		LockSystem: s.lock,
		Logger:     webdavLogger,
	}

	s.mu.Lock()
	s.sess[user] = se
	s.mu.Unlock()
	return se, nil
}

func webdavLogger(r *http.Request, err error) {
	if err != nil {
		log.Printf("webdav %s %s: %v", r.Method, r.URL.Path, err)
	}
}
