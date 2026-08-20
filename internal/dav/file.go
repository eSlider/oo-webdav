package dav

import (
	"bytes"
	"context"
	"io"
	"mime"
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

func (i nodeInfo) Name() string       { return i.n.name }
func (i nodeInfo) Size() int64        { return i.n.size }
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

// dirFile is a directory handle; Readdir returns cached children.
type dirFile struct {
	node  *node
	infos []os.FileInfo
	idx   int
}

func (d *dirFile) Readdir(count int) ([]os.FileInfo, error) {
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
	parent   string
	existing *node // non-nil when overwriting an existing file
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
	if w.buf.Len() == 0 {
		// The portal rejects empty uploads with "Empty file". Skip 0-byte PUTs
		// (e.g. Office `.~lock.*` files, which Windows creates empty when
		// opening a document) and report success. Such files carry no portal
		// content, so nothing is lost; non-empty updates are uploaded normally.
		w.fs.invalidate(w.parent)
		return nil
	}
	ctx := context.Background()
	// Upload the buffered contents to the parent folder. OnlyOffice creates a
	// new version when a file with the same title already exists, which
	// matches WebDAV PUT overwrite semantics.
	_, err := w.fs.client.UploadDavFile(ctx, w.parentID, w.name, &w.buf)
	if err != nil {
		return err
	}
	w.fs.invalidate(w.parent)
	return nil
}
