package dav

import (
	"context"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eslider/go-onlyoffice"
)

// fs implements golang.org/x/net/webdav.FileSystem by mapping the virtual
// file tree onto the ONLYOFFICE Files module for a single authenticated user.
// The virtual root "/" maps to the portal's "@root" sections.
type fs struct {
	client *onlyoffice.Client
	rootID string // portal folder id backing the WebDAV root "/"
	ttl    time.Duration

	mu     sync.Mutex
	cached map[string]*cacheEntry // key: parent dir virtual path ("/" for root)
	rootN  string                 // resolved numeric id of the root ("" if unresolved)
}

type cacheEntry struct {
	listing *onlyoffice.DavListing
	fetched time.Time
}

// node describes one item in the virtual tree.
type node struct {
	path     string // virtual path, e.g. "/a/b.txt"
	name     string
	isDir    bool
	id       string // folderId or fileId; "@root" for the root directory
	parentID string // folder id of the parent ("" for root)
	size     int64
	mtime    time.Time
}

func newFS(client *onlyoffice.Client, rootID string, ttl time.Duration) *fs {
	if rootID == "" {
		rootID = "@root"
	}
	return &fs{client: client, rootID: rootID, ttl: ttl, cached: make(map[string]*cacheEntry)}
}

// rootIsSections reports whether the WebDAV root is the aggregated view of the
// portal's virtual sections ("In projects", "My documents", ...) rather than a
// single writable folder like "@my".
func (f *fs) rootIsSections() bool { return f.rootID == "@root" }

// invalidate drops cached listings at or below p (the virtual directory path).
func (f *fs) invalidate(p string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k := range f.cached {
		if k == p || strings.HasPrefix(k, p+"/") {
			delete(f.cached, k)
		}
	}
}

func (f *fs) listing(ctx context.Context, dirPath string) (*onlyoffice.DavListing, error) {
	if dirPath == "" {
		dirPath = "/"
	}
	f.mu.Lock()
	if e, ok := f.cached[dirPath]; ok && time.Since(e.fetched) < f.ttl {
		f.mu.Unlock()
		return e.listing, nil
	}
	f.mu.Unlock()

	if dirPath == "/" && f.rootIsSections() {
		sections, err := f.client.ListDavSections(ctx)
		if err != nil {
			return nil, err
		}
		l := &onlyoffice.DavListing{Folders: sections}
		f.mu.Lock()
		f.cached["/"] = &cacheEntry{listing: l, fetched: time.Now()}
		f.mu.Unlock()
		return l, nil
	}

	id, err := f.dirID(ctx, dirPath)
	if err != nil {
		return nil, err
	}
	l, err := f.client.ListDavFolder(ctx, id)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.cached[dirPath] = &cacheEntry{listing: l, fetched: time.Now()}
	f.mu.Unlock()
	return l, nil
}

// numericID resolves a folder id to a numeric id usable in mutating API calls.
// Symbolic roots like "@my" are resolved via their listing; numeric ids pass
// through unchanged.
func (f *fs) numericID(ctx context.Context, id string) (string, error) {
	if _, err := strconv.Atoi(id); err == nil {
		return id, nil
	}
	l, err := f.client.ListDavFolder(ctx, id)
	if err != nil {
		return "", err
	}
	if l.Current.ID == "" {
		return "", &os.PathError{Op: "resolve", Path: id, Err: os.ErrInvalid}
	}
	return l.Current.ID, nil
}

// dirID returns the folder id for a virtual directory path. Symbolic roots are
// resolved to their numeric id so mutating operations work; the aggregated
// "@root" root is left symbolic (creating folders at the very top is not
// supported by the portal, matching the original WebDAV).
func (f *fs) dirID(ctx context.Context, dirPath string) (string, error) {
	if dirPath == "" || dirPath == "/" {
		if f.rootIsSections() {
			return "@root", nil
		}
		f.mu.Lock()
		if f.rootN != "" {
			id := f.rootN
			f.mu.Unlock()
			return id, nil
		}
		f.mu.Unlock()
		id, err := f.numericID(ctx, f.rootID)
		if err != nil {
			return "", err
		}
		f.mu.Lock()
		f.rootN = id
		f.mu.Unlock()
		return id, nil
	}
	n, err := f.resolve(ctx, dirPath)
	if err != nil {
		return "", err
	}
	if !n.isDir {
		return "", &os.PathError{Op: "stat", Path: dirPath, Err: os.ErrNotExist}
	}
	return n.id, nil
}

// resolve walks the virtual path from the root to return its node. The path is
// cleaned and must start with "/".
func (f *fs) resolve(ctx context.Context, name string) (*node, error) {
	name = cleanPath(name)
	if name == "/" {
		return &node{path: "/", name: "/", isDir: true, id: f.rootID, mtime: time.Now()}, nil
	}

	// Walk from root, listing each ancestor to find the next child id.
	dir := "/"
	segs := strings.Split(strings.TrimPrefix(name, "/"), "/")
	for i, seg := range segs {
		l, err := f.listing(ctx, dir)
		if err != nil {
			return nil, err
		}
		n := findChild(l, seg)
		if n == nil {
			return nil, &os.PathError{Op: "stat", Path: name, Err: os.ErrNotExist}
		}
		if i == len(segs)-1 {
			return n, nil
		}
		if !n.isDir {
			return nil, &os.PathError{Op: "stat", Path: name, Err: os.ErrNotExist}
		}
		dir = path.Join(dir, seg)
	}
	return nil, &os.PathError{Op: "stat", Path: name, Err: os.ErrNotExist}
}

// parentOf returns the virtual path of name's parent directory.
func parentOf(name string) string {
	name = cleanPath(name)
	if name == "/" {
		return "/"
	}
	d := path.Dir(name)
	if d == "." || d == "" {
		return "/"
	}
	return d
}

// cleanPath normalizes a virtual path to a clean "/"-rooted path.
func cleanPath(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = path.Clean("/" + name)
	if name == "." || name == "" {
		return "/"
	}
	return name
}

// findChild locates a child by name within a listing, preferring files.
func findChild(l *onlyoffice.DavListing, name string) *node {
	for i := range l.Folders {
		fd := &l.Folders[i]
		if fd.Title == name {
			return &node{
				name:     fd.Title,
				isDir:    true,
				id:       fd.ID,
				size:     0,
				mtime:    fd.ModTime(),
				parentID: fd.ParentID,
			}
		}
	}
	for i := range l.Files {
		fi := &l.Files[i]
		if fi.Title == name {
			return &node{
				name:  fi.Title,
				isDir: false,
				id:    fi.ID,
				size:  fi.Size,
				mtime: fi.ModTime(),
			}
		}
	}
	return nil
}

// childrenOf returns the child nodes (FileInfo) of a directory path.
func (f *fs) childrenOf(ctx context.Context, dirPath string) ([]os.FileInfo, error) {
	l, err := f.listing(ctx, dirPath)
	if err != nil {
		return nil, err
	}
	infos := make([]os.FileInfo, 0, len(l.Files)+len(l.Folders))
	for i := range l.Folders {
		fd := &l.Folders[i]
		infos = append(infos, nodeInfo{&node{
			name:  fd.Title,
			isDir: true,
			id:    fd.ID,
			mtime: fd.ModTime(),
		}})
	}
	for i := range l.Files {
		fi := &l.Files[i]
		infos = append(infos, nodeInfo{&node{
			name:  fi.Title,
			isDir: false,
			id:    fi.ID,
			size:  fi.Size,
			mtime: fi.ModTime(),
		}})
	}
	sort.Slice(infos, func(i, j int) bool {
		return infos[i].Name() < infos[j].Name()
	})
	return infos, nil
}

// Stat implements webdav.FileSystem.
func (f *fs) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	n, err := f.resolve(ctx, name)
	if err != nil {
		return nil, err
	}
	return nodeInfo{n}, nil
}

// Mkdir implements webdav.FileSystem.
func (f *fs) Mkdir(ctx context.Context, name string, _ os.FileMode) error {
	name = cleanPath(name)
	parent := parentOf(name)
	pid, err := f.dirID(ctx, parent)
	if err != nil {
		return err
	}
	if _, err := f.client.CreateDavFolder(ctx, pid, path.Base(name)); err != nil {
		return err
	}
	f.invalidate(parent)
	return nil
}

// OpenFile implements webdav.FileSystem.
func (f *fs) OpenFile(ctx context.Context, name string, flag int, _ os.FileMode) (fileT, error) {
	name = cleanPath(name)
	isReadOnly := flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC) == 0

	if isReadOnly {
		n, err := f.resolve(ctx, name)
		if err != nil {
			return nil, err
		}
		if n.isDir {
			infos, err := f.childrenOf(ctx, name)
			if err != nil {
				return nil, err
			}
			return &dirFile{node: n, infos: infos}, nil
		}
		// Reading a file: stream to a temp file so Seek/Range work.
		return f.openReadFile(ctx, n)
	}

	// Write path (create/truncate/append).
	return f.openWriteFile(ctx, name)
}

func (f *fs) openReadFile(ctx context.Context, n *node) (fileT, error) {
	// Defer the download until content is actually read (GET / range requests).
	// Metadata-only operations (PROPFIND) never download the file body.
	return &readFile{fs: f, ctx: ctx, node: n}, nil
}

func (f *fs) openWriteFile(ctx context.Context, name string) (fileT, error) {
	parent := parentOf(name)
	pid, err := f.dirID(ctx, parent)
	if err != nil {
		return nil, err
	}
	// Determine whether we're overwriting an existing file.
	var existing *node
	if n, err := f.resolve(ctx, name); err == nil && !n.isDir {
		existing = n
	}
	return &writeFile{
		fs:       f,
		parentID: pid,
		name:     path.Base(name),
		parent:   parent,
		existing: existing,
	}, nil
}

// RemoveAll implements webdav.FileSystem.
func (f *fs) RemoveAll(ctx context.Context, name string) error {
	name = cleanPath(name)
	if name == "/" {
		return &os.PathError{Op: "remove", Path: name, Err: os.ErrPermission}
	}
	n, err := f.resolve(ctx, name)
	if err != nil {
		return err
	}
	var fIDs, dIDs []string
	if n.isDir {
		dIDs = []string{n.id}
	} else {
		fIDs = []string{n.id}
	}
	if err := f.client.DeleteDavItems(ctx, dIDs, fIDs); err != nil {
		return err
	}
	f.invalidate(parentOf(name))
	return nil
}

// Rename implements webdav.FileSystem (move to new path).
func (f *fs) Rename(ctx context.Context, oldName, newName string) error {
	oldName = cleanPath(oldName)
	newName = cleanPath(newName)
	n, err := f.resolve(ctx, oldName)
	if err != nil {
		return err
	}
	destParent := parentOf(newName)
	destID, err := f.dirID(ctx, destParent)
	if err != nil {
		return err
	}
	base := path.Base(newName)
	if base != n.name && n.isDir {
		if err := f.client.RenameDavFolder(ctx, n.id, base); err != nil {
			return err
		}
	} else if base != n.name {
		if err := f.client.RenameDavFile(ctx, n.id, base); err != nil {
			return err
		}
	}
	if destParent != parentOf(oldName) {
		var fIDs, dIDs []string
		if n.isDir {
			dIDs = []string{n.id}
		} else {
			fIDs = []string{n.id}
		}
		if err := f.client.MoveDavItems(ctx, dIDs, fIDs, destID); err != nil {
			return err
		}
	}
	f.invalidate(parentOf(oldName))
	f.invalidate(destParent)
	return nil
}
