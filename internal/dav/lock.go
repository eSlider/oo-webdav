package dav

import (
	"fmt"
	"sync"
	"time"

	"golang.org/x/net/webdav"
)

// permissiveLockSystem implements webdav.LockSystem but never blocks an
// operation. Windows WebDAV clients (and Office) issue LOCK/UNLOCK and then
// frequently write, rename or move without echoing the lock token; the strict
// webdav.NewMemLS rejects those with 423 Locked, which breaks Office atomic
// saves (temp file -> MOVE over the target) and rename flows. Locks are minted
// and remembered so LOCK/UNLOCK/refresh succeed, but Confirm always allows the
// request. Real conflict handling stays with the ONLYOFFICE portal.
type permissiveLockSystem struct {
	mu      sync.Mutex
	byToken map[string]webdav.LockDetails
	gen     uint64
}

func newPermissiveLockSystem() webdav.LockSystem {
	return &permissiveLockSystem{byToken: make(map[string]webdav.LockDetails)}
}

// Confirm always grants the request and returns a no-op release.
func (l *permissiveLockSystem) Confirm(time.Time, string, string, ...webdav.Condition) (func(), error) {
	return func() {}, nil
}

// Create mints an opaque token so the client can echo it back in If headers.
func (l *permissiveLockSystem) Create(now time.Time, details webdav.LockDetails) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.gen++
	token := fmt.Sprintf("opaquelocktoken:%x-%x", now.UnixNano(), l.gen)
	l.byToken[token] = details
	return token, nil
}

// Refresh never fails: unknown tokens get an infinite default so a stale client
// re-lock does not surface as a 412.
func (l *permissiveLockSystem) Refresh(_ time.Time, token string, _ time.Duration) (webdav.LockDetails, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if d, ok := l.byToken[token]; ok {
		return d, nil
	}
	return webdav.LockDetails{Duration: -1}, nil
}

// Unlock is a no-op for unknown tokens (a client may unlock after a restart).
func (l *permissiveLockSystem) Unlock(_ time.Time, token string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.byToken, token)
	return nil
}
