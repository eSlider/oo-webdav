package dav

import (
	"bytes"
	"context"
	"os"
	"path"
	"path/filepath"
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
	client  *onlyoffice.Client
	rootID  string // portal folder id backing the WebDAV root "/"
	ttl     time.Duration
	rootTTL time.Duration // longer TTL for the expensive @root sections listing

	mu     sync.Mutex
	cached map[string]*cacheEntry // key: parent dir virtual path ("/" for root)
	rootN  string                 // resolved numeric id of the root ("" if unresolved)

	// hidden is a per-session overlay for Office/OS marker files (~$…,
	// .~lock.*, desktop.ini, …). They stay visible to WebDAV clients by exact
	// path but are never written to the portal and never appear in listings.
	hidden map[string]*hiddenEntry

	// recent records paths this session created/renamed, with the portal file id,
	// for a short window. It makes creation idempotent (a follow-up LOCK then PUT
	// for the same new name must not create a second file while the portal folder
	// listing is still catching up) and lets an immediate overwrite update the
	// just-created file by id instead of uploading a duplicate.
	recent map[string]recentFile

	// gator holds gator document fields by ONLYOFFICE file id, exposed as dead
	// properties. nil when the feature is disabled.
	gator *gatorIndex
}

// recentFile is a remembered create/rename: portal id plus when it happened.
type recentFile struct {
	id string
	at time.Time
}

// recentCreateTTL bounds how long a create is remembered for duplicate
// suppression.
const recentCreateTTL = 2 * time.Minute

// markCreated records that name was created in the portal with the given id.
func (f *fs) markCreated(name, id string) {
	name = cleanPath(name)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.recent == nil {
		f.recent = make(map[string]recentFile)
	}
	now := time.Now()
	for k, r := range f.recent {
		if now.Sub(r.at) > recentCreateTTL {
			delete(f.recent, k)
		}
	}
	f.recent[name] = recentFile{id: id, at: now}
}

// wasRecentlyCreated returns the portal id of a name created in this session
// within the TTL window.
func (f *fs) wasRecentlyCreated(name string) (string, bool) {
	name = cleanPath(name)
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.recent[name]
	if !ok {
		return "", false
	}
	if time.Since(r.at) > recentCreateTTL {
		delete(f.recent, name)
		return "", false
	}
	return r.id, true
}

func (f *fs) unmarkCreated(name string) {
	name = cleanPath(name)
	f.mu.Lock()
	delete(f.recent, name)
	f.mu.Unlock()
}

// hiddenEntry is one overlay file (content + mtime).
type hiddenEntry struct {
	content []byte
	mtime   time.Time
}

// isHiddenName reports whether a file name belongs to the hidden overlay. These
// are Office/OS lock and shell-metadata files: clients must still be able to
// create, read, stat and delete them, but users should not see them.
func isHiddenName(name string) bool {
	base := strings.ToLower(path.Base(name))
	if strings.HasPrefix(base, "~$") || strings.HasPrefix(base, ".~lock.") {
		return true
	}
	switch base {
	case "desktop.ini", "thumbs.db", "autorun.inf":
		return true
	}
	return false
}

func (f *fs) hiddenGet(name string) (*hiddenEntry, bool) {
	name = cleanPath(name)
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.hidden[name]
	return e, ok
}

func (f *fs) hiddenPut(name string, content []byte) {
	name = cleanPath(name)
	cp := make([]byte, len(content))
	copy(cp, content)
	f.mu.Lock()
	f.hidden[name] = &hiddenEntry{content: cp, mtime: time.Now()}
	f.mu.Unlock()
}

func (f *fs) hiddenDelete(name string) bool {
	name = cleanPath(name)
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.hidden[name]; ok {
		delete(f.hidden, name)
		return true
	}
	return false
}

func (f *fs) hiddenRename(oldName, newName string) bool {
	oldName = cleanPath(oldName)
	newName = cleanPath(newName)
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.hidden[oldName]
	if !ok {
		return false
	}
	delete(f.hidden, oldName)
	f.hidden[newName] = e
	return true
}

// hiddenNode adapts an overlay file to the node type.
func hiddenNode(name string, e *hiddenEntry) *node {
	return &node{
		path:  cleanPath(name),
		name:  path.Base(cleanPath(name)),
		isDir: false,
		size:  int64(len(e.content)),
		mtime: e.mtime,
	}
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

func newFS(client *onlyoffice.Client, rootID string, ttl, rootTTL time.Duration) *fs {
	if rootID == "" {
		rootID = "@root"
	}
	if rootTTL <= 0 {
		rootTTL = ttl
	}
	return &fs{client: client, rootID: rootID, ttl: ttl, rootTTL: rootTTL, cached: make(map[string]*cacheEntry), hidden: make(map[string]*hiddenEntry), recent: make(map[string]recentFile)}
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
	ttl := f.ttl
	if dirPath == "/" && f.rootIsSections() {
		ttl = f.rootTTL
	}
	f.mu.Lock()
	if e, ok := f.cached[dirPath]; ok && time.Since(e.fetched) < ttl {
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

// findChild locates a child by name within a listing, preferring folders on an
// exact title match. When no exact match exists it falls back to a trailing
// dot/space match: Windows strips trailing dots and spaces from names before
// sending them, so a folder stored as "Neuer Ordner." is otherwise unreachable
// (and undeletable) by its stripped name "Neuer Ordner".
func findChild(l *onlyoffice.DavListing, name string) *node {
	if n := findChildExact(l, name); n != nil {
		return n
	}
	return findChildTrimmed(l, name)
}

func findChildExact(l *onlyoffice.DavListing, name string) *node {
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

// findChildTrimmed matches a child whose title equals name after trimming
// trailing dots and spaces. It returns a match only when it is unique, so an
// ambiguous folder ("Neuer Ordner." and "Neuer Ordner ") is not guessed.
func findChildTrimmed(l *onlyoffice.DavListing, name string) *node {
	want := strings.TrimRight(name, ". ")
	var matches []*node
	for i := range l.Folders {
		fd := &l.Folders[i]
		if strings.TrimRight(fd.Title, ". ") == want {
			matches = append(matches, &node{name: fd.Title, isDir: true, id: fd.ID, mtime: fd.ModTime(), parentID: fd.ParentID})
		}
	}
	for i := range l.Files {
		fi := &l.Files[i]
		if strings.TrimRight(fi.Title, ". ") == want {
			matches = append(matches, &node{name: fi.Title, isDir: false, id: fi.ID, size: fi.Size, mtime: fi.ModTime()})
		}
	}
	if len(matches) == 1 {
		return matches[0]
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
		if isHiddenName(fd.Title) {
			continue
		}
		infos = append(infos, nodeInfo{&node{
			name:  fd.Title,
			isDir: true,
			id:    fd.ID,
			mtime: fd.ModTime(),
		}})
	}
	for i := range l.Files {
		fi := &l.Files[i]
		if isHiddenName(fi.Title) {
			continue
		}
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
	name = cleanPath(name)
	if isHiddenName(name) {
		if e, ok := f.hiddenGet(name); ok {
			return nodeInfo{hiddenNode(name, e)}, nil
		}
	}
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

// emptyCreateKind selects the portal endpoint used to materialise a 0-byte file.
type emptyCreateKind int

const (
	createText emptyCreateKind = iota
	createHTML
	createFileExternal
	createFileInternal
)

// emptyCreateFor maps a file name to the portal create endpoint required for a
// 0-byte write. The portal cannot store an empty upload, so a file record is
// created first (mirrors ASC.WebDav customVirtualResource.create):
//
//	.txt               -> POST .../text
//	.html/.htm         -> POST .../html
//	.docx/.xlsx/.pptx  -> POST .../file (template, EnableExternalExt=false)
//	anything else      -> POST .../file (EnableExternalExt=true, keep extension)
func emptyCreateFor(name string) emptyCreateKind {
	switch strings.ToLower(strings.TrimPrefix(filepath.Ext(name), ".")) {
	case "txt":
		return createText
	case "html", "htm":
		return createHTML
	case "docx", "xlsx", "pptx":
		return createFileInternal
	default:
		return createFileExternal
	}
}

// createEmptyFile materialises a 0-byte file in the portal via the endpoint
// chosen by extension. The normal upload path rejects empty bodies.
func (f *fs) createEmptyFile(ctx context.Context, parentID, name string) (*onlyoffice.DavFile, error) {
	switch emptyCreateFor(name) {
	case createText:
		return f.client.CreateDavTextFile(ctx, parentID, name)
	case createHTML:
		return f.client.CreateDavHtmlFile(ctx, parentID, name)
	case createFileInternal:
		return f.client.CreateDavFile(ctx, parentID, name, false)
	default:
		return f.client.CreateDavFile(ctx, parentID, name, true)
	}
}

// copyDavFile copies the content of srcID to a new file named destName in
// destID and returns it. Used for a rename that changes the extension: the
// portal's rename keeps a file's original extension, so a temp (.tmp) could not
// otherwise become a .txt/.docx target.
func (f *fs) copyDavFile(ctx context.Context, srcID, destID, destName string) (*onlyoffice.DavFile, error) {
	var buf bytes.Buffer
	if _, err := f.client.DownloadDavFile(ctx, srcID, &buf); err != nil {
		return nil, err
	}
	return f.client.UploadDavFile(ctx, destID, destName, &buf)
}

// OpenFile implements webdav.FileSystem.
func (f *fs) OpenFile(ctx context.Context, name string, flag int, _ os.FileMode) (fileT, error) {
	name = cleanPath(name)
	isReadOnly := flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC) == 0

	if isReadOnly {
		if isHiddenName(name) {
			if e, ok := f.hiddenGet(name); ok {
				return &hiddenReadFile{n: hiddenNode(name, e), content: e.content}, nil
			}
		}
		n, err := f.resolve(ctx, name)
		if err != nil {
			return nil, err
		}
		if n.isDir {
			// Lazy: do not enumerate the directory here. PROPFIND opens each
			// node (including child directories) just to read properties, so
			// eager listing here made a Depth-1 PROPFIND fetch every subfolder.
			// childrenOf runs on the first Readdir instead.
			return &dirFile{fs: f, ctx: ctx, node: n, path: name}, nil
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
	name = cleanPath(name)
	parent := parentOf(name)
	if isHiddenName(name) {
		// Office/OS marker files stay in the per-session overlay.
		return &writeFile{fs: f, parent: parent, path: name, name: path.Base(name), hidden: true}, nil
	}
	pid, err := f.dirID(ctx, parent)
	if err != nil {
		return nil, err
	}
	// Determine whether we're overwriting an existing file. If the portal
	// listing has not caught up with a create/rename from this session yet, fall
	// back to the remembered id so we update that file instead of duplicating it.
	var existing *node
	if n, err := f.resolve(ctx, name); err == nil && !n.isDir {
		existing = n
	} else if id, ok := f.wasRecentlyCreated(name); ok {
		existing = &node{path: name, name: path.Base(name), isDir: false, id: id}
	}
	return &writeFile{
		fs:       f,
		parentID: pid,
		name:     path.Base(name),
		path:     name,
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
	if isHiddenName(name) && f.hiddenDelete(name) {
		f.invalidate(parentOf(name))
		return nil
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
	f.unmarkCreated(name)
	f.invalidate(parentOf(name))
	return nil
}

// Rename implements webdav.FileSystem (move to new path).
func (f *fs) Rename(ctx context.Context, oldName, newName string) error {
	oldName = cleanPath(oldName)
	newName = cleanPath(newName)

	oldHidden := isHiddenName(oldName)
	newHidden := isHiddenName(newName)

	// Hidden -> hidden: move inside the overlay.
	if oldHidden && newHidden && f.hiddenRename(oldName, newName) {
		return nil
	}
	// Hidden -> real: materialise the overlay content into the portal (Office
	// atomic save writes a temp/marker then renames it onto the target).
	if oldHidden && !newHidden {
		if e, ok := f.hiddenGet(oldName); ok {
			destParent := parentOf(newName)
			destID, err := f.dirID(ctx, destParent)
			if err != nil {
				return err
			}
			base := path.Base(newName)
			var created *onlyoffice.DavFile
			if len(e.content) > 0 {
				created, err = f.client.UploadDavFile(ctx, destID, base, bytes.NewReader(e.content))
				if err != nil {
					return err
				}
			} else {
				created, err = f.createEmptyFile(ctx, destID, base)
				if err != nil {
					return err
				}
			}
			if created != nil {
				f.markCreated(newName, created.ID)
			}
			f.hiddenDelete(oldName)
			f.invalidate(destParent)
			return nil
		}
	}

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

	extChanged := !n.isDir && !strings.EqualFold(path.Ext(base), path.Ext(n.name))
	if extChanged {
		// The portal's rename preserves the source extension, so a .tmp could
		// never become a .txt/.docx target. Copy the content to a file with the
		// target name and remove the source (Office/editor safe-save path).
		created, err := f.copyDavFile(ctx, n.id, destID, base)
		if err != nil {
			return err
		}
		if err := f.client.DeleteDavItems(ctx, nil, []string{n.id}); err != nil {
			return err
		}
		if created != nil {
			f.markCreated(newName, created.ID)
		}
	} else {
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
		if !n.isDir {
			f.markCreated(newName, n.id)
		}
	}
	f.unmarkCreated(oldName)
	f.invalidate(parentOf(oldName))
	f.invalidate(destParent)
	return nil
}
