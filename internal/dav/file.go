package dav

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/net/webdav"
)

// fileT aliases the webdav.File interface returned by OpenFile.
type fileT = webdav.File

// nodeInfo adapts a node to os.FileInfo.
type nodeInfo struct {
	n *node
}

func (i nodeInfo) Name() string { return i.n.name }
func (i nodeInfo) Size() int64  { return i.n.size }
func (i nodeInfo) Mode() os.FileMode {
	if i.n.isDir {
		return 0555 | os.ModeDir
	}
	return 0444
}
func (i nodeInfo) ModTime() time.Time { return i.n.mtime }
func (i nodeInfo) IsDir() bool        { return i.n.isDir }
func (i nodeInfo) Sys() any           { return nil }

// ContentType satisfies webdav.ContentTyper so PROPFIND/GET report the MIME
// type by extension without downloading the file body to sniff it.
func (i nodeInfo) ContentType(_ context.Context) (string, error) {
	if ct := mime.TypeByExtension(strings.ToLower(filepath.Ext(i.n.name))); ct != "" {
		return ct, nil
	}
	return "application/octet-stream", nil
}

// readFile is a lazily-loaded file. Its content is downloaded from the portal
// into a temp file on first Read/Seek; Stat returns the portal node metadata
// without downloading, so PROPFIND does not fetch file bodies.
type readFile struct {
	fs      *fs
	ctx     context.Context
	node    *node
	loaded  bool
	f       *os.File
	tmpPath string
}

func (r *readFile) ensureLoaded() error {
	if r.loaded {
		return nil
	}
	tmp, err := os.CreateTemp("", "ooshare-r-*")
	if err != nil {
		return err
	}
	if _, err := r.fs.client.DownloadDavFile(r.ctx, r.node.id, tmp); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if _, err := tmp.Seek(0, 0); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	r.f = tmp
	r.tmpPath = tmp.Name()
	r.loaded = true
	return nil
}

func (r *readFile) Readdir(count int) ([]os.FileInfo, error) { return nil, webdav.ErrNotImplemented }

// Stat reports the portal node's metadata (name/size/mtime), not the backing
// temp file, so WebDAV properties are correct and no download is triggered.
func (r *readFile) Stat() (os.FileInfo, error) {
	if r.node != nil {
		return nodeInfo{r.node}, nil
	}
	if r.loaded {
		return r.f.Stat()
	}
	return nil, webdav.ErrNotImplemented
}
func (r *readFile) Close() error {
	if r.loaded {
		err := r.f.Close()
		os.Remove(r.tmpPath)
		return err
	}
	return nil
}
func (r *readFile) Read(p []byte) (int, error) {
	if err := r.ensureLoaded(); err != nil {
		return 0, err
	}
	return r.f.Read(p)
}
func (r *readFile) Seek(o int64, w int) (int64, error) {
	if err := r.ensureLoaded(); err != nil {
		return 0, err
	}
	return r.f.Seek(o, w)
}
func (r *readFile) Write(p []byte) (int, error) { return 0, webdav.ErrNotImplemented }

// DeadProps implements webdav.DeadPropsHolder: it exposes gator document fields
// as namespaced read-only properties so WebDAV clients can display and filter
// them. Returns nothing when gator has no record for the file (feature off, the
// document was never OCR'd, or it has no oo_file_id link).
func (r *readFile) DeadProps() (map[xml.Name]webdav.Property, error) {
	if r.node == nil {
		return nil, nil
	}
	p, ok := r.fs.gator.lookup(r.node.id)
	if !ok {
		return nil, nil
	}
	values := gatorPropValues(p)
	out := make(map[xml.Name]webdav.Property, len(gatorPropNames))
	for _, local := range gatorPropNames {
		v := values[local]
		if v == "" {
			continue
		}
		name := xml.Name{Space: gatorNamespace, Local: local}
		out[name] = webdav.Property{XMLName: name, InnerXML: []byte(escapeXML(v))}
	}
	return out, nil
}

// Patch implements webdav.DeadPropsHolder. Gator properties are read-only, so
// every patch is forbidden (same as a resource without dead properties).
func (r *readFile) Patch(patches []webdav.Proppatch) ([]webdav.Propstat, error) {
	pstat := webdav.Propstat{Status: http.StatusForbidden}
	for _, patch := range patches {
		for _, p := range patch.Props {
			pstat.Props = append(pstat.Props, webdav.Property{XMLName: p.XMLName})
		}
	}
	return []webdav.Propstat{pstat}, nil
}

// escapeXML escapes a value for use as XML element content.
func escapeXML(s string) string {
	if !strings.ContainsAny(s, "&<>") {
		return s
	}
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// dirFile is a directory handle; children are fetched lazily on the first
// Readdir so PROPFIND/Stat-only callers (which open every node) do not trigger
// a portal listing per directory.
type dirFile struct {
	fs     *fs
	ctx    context.Context
	node   *node
	path   string
	infos  []os.FileInfo
	loaded bool
	idx    int
}

func (d *dirFile) load() error {
	if d.loaded {
		return nil
	}
	d.loaded = true
	infos, err := d.fs.childrenOf(d.ctx, d.path)
	if err != nil {
		return err
	}
	d.infos = infos
	return nil
}

func (d *dirFile) Readdir(count int) ([]os.FileInfo, error) {
	if err := d.load(); err != nil {
		return nil, err
	}
	if d.idx >= len(d.infos) && count > 0 {
		return nil, io.EOF
	}
	if count <= 0 {
		out := d.infos[d.idx:]
		d.idx = len(d.infos)
		return out, nil
	}
	end := d.idx + count
	if end > len(d.infos) {
		end = len(d.infos)
	}
	out := d.infos[d.idx:end]
	d.idx = end
	return out, nil
}
func (d *dirFile) Stat() (os.FileInfo, error) {
	if d.node == nil {
		return nil, webdav.ErrNotImplemented
	}
	return nodeInfo{d.node}, nil
}
func (d *dirFile) Close() error               { return nil }
func (d *dirFile) Read(p []byte) (int, error) { return 0, webdav.ErrNotImplemented }
func (d *dirFile) Seek(o int64, w int) (int64, error) {
	return 0, webdav.ErrNotImplemented
}
func (d *dirFile) Write(p []byte) (int, error) { return 0, webdav.ErrNotImplemented }

// writeFile buffers writes and uploads the whole file on Close.
type writeFile struct {
	fs       *fs
	parentID string
	name     string
	path     string
	parent   string
	existing *node // non-nil when overwriting an existing file
	hidden   bool  // hidden overlay file (Office/OS marker)
	buf      bytes.Buffer
	closed   bool
}

func (w *writeFile) Readdir(count int) ([]os.FileInfo, error) {
	return nil, webdav.ErrNotImplemented
}

// Stat reports the in-flight write buffer so webdav can compute an ETag.
func (w *writeFile) Stat() (os.FileInfo, error) {
	return nodeInfo{&node{name: w.name, isDir: false, size: int64(w.buf.Len()), mtime: time.Now()}}, nil
}
func (w *writeFile) Read(p []byte) (int, error) { return 0, webdav.ErrNotImplemented }
func (w *writeFile) Seek(o int64, s int) (int64, error) {
	return 0, webdav.ErrNotImplemented
}

func (w *writeFile) Write(p []byte) (int, error) {
	if w.closed {
		return 0, os.ErrClosed
	}
	return w.buf.Write(p)
}

func (w *writeFile) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	if w.hidden {
		// Office/OS marker files stay in the per-session overlay: clients can
		// stat/get/put/delete them, but they never reach the portal and never
		// appear in listings.
		w.fs.hiddenPut(w.path, w.buf.Bytes())
		return nil
	}
	if w.buf.Len() == 0 {
		// The portal rejects empty uploads ("Empty file"). For a new file,
		// create the portal record via the extension-appropriate endpoint so
		// the file exists: Windows Explorer "New -> Text Document"/Bitmap etc.
		// is a 0-byte PUT. A follow-up open for the same name (LOCK creates the
		// resource, then PUT) is recognised via existing/recent and skipped, so
		// the file is not created twice.
		if w.existing == nil {
			created, err := w.fs.createEmptyFile(context.Background(), w.parentID, w.name)
			if err != nil {
				return err
			}
			if created != nil {
				w.fs.markCreated(w.path, created.ID)
			}
		}
		w.fs.invalidate(w.parent)
		return nil
	}
	ctx := context.Background()
	// Overwrite in place by file id. The /upload endpoint creates a second file
	// with the same title, so it must only be used for genuinely new files.
	if w.existing != nil && w.existing.id != "" {
		if _, err := w.fs.client.UpdateDavFile(ctx, w.existing.id, w.name, &w.buf); err != nil {
			return err
		}
		w.fs.markCreated(w.path, w.existing.id)
	} else {
		created, err := w.fs.client.UploadDavFile(ctx, w.parentID, w.name, &w.buf)
		if err != nil {
			return err
		}
		if created != nil {
			w.fs.markCreated(w.path, created.ID)
		}
	}
	w.fs.invalidate(w.parent)
	return nil
}

// hiddenReadFile serves an overlay file (Office/OS marker) from memory.
type hiddenReadFile struct {
	n       *node
	content []byte
	off     int64
}

func (r *hiddenReadFile) Readdir(int) ([]os.FileInfo, error) { return nil, webdav.ErrNotImplemented }
func (r *hiddenReadFile) Stat() (os.FileInfo, error)         { return nodeInfo{r.n}, nil }
func (r *hiddenReadFile) Close() error                       { return nil }
func (r *hiddenReadFile) Write([]byte) (int, error)          { return 0, webdav.ErrNotImplemented }

func (r *hiddenReadFile) Read(p []byte) (int, error) {
	if r.off >= int64(len(r.content)) {
		return 0, io.EOF
	}
	n := copy(p, r.content[r.off:])
	r.off += int64(n)
	return n, nil
}

func (r *hiddenReadFile) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		r.off = offset
	case io.SeekCurrent:
		r.off += offset
	case io.SeekEnd:
		r.off = int64(len(r.content)) + offset
	}
	if r.off < 0 {
		r.off = 0
	}
	return r.off, nil
}
