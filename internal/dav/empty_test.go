package dav

import "testing"

func TestIsHiddenName(t *testing.T) {
	hidden := []string{
		"~$report.docx",
		".~lock.report.docx#",
		"desktop.ini",
		"Desktop.INI",
		"Thumbs.db",
		"AutoRun.inf",
		"a/Desktop.ini",
	}
	for _, name := range hidden {
		if !isHiddenName(name) {
			t.Errorf("isHiddenName(%q) = false, want true", name)
		}
	}
	real := []string{
		"Textdokument (neu).txt",
		"Bitmap-Bild (neu).bmp",
		"notes.txt",
		"report.docx",
		"bank.xlsx",
		"index.html",
		"archive.zip",
		"noext",
	}
	for _, name := range real {
		if isHiddenName(name) {
			t.Errorf("isHiddenName(%q) = true, want false", name)
		}
	}
}

func TestHiddenOverlay(t *testing.T) {
	f := newFS(nil, "@root", 0, 0)
	// absent
	if _, ok := f.hiddenGet("/a/~$x.docx"); ok {
		t.Fatal("hiddenGet on empty overlay = ok")
	}
	// put + get
	f.hiddenPut("/a/~$x.docx", []byte("owner"))
	e, ok := f.hiddenGet("/a/~$x.docx")
	if !ok || string(e.content) != "owner" {
		t.Fatalf("hiddenGet = %v %v, want ok owner", ok, e)
	}
	// rename inside overlay
	if !f.hiddenRename("/a/~$x.docx", "/a/~$y.docx") {
		t.Fatal("hiddenRename returned false")
	}
	if _, ok := f.hiddenGet("/a/~$x.docx"); ok {
		t.Fatal("old hidden path still present after rename")
	}
	if _, ok := f.hiddenGet("/a/~$y.docx"); !ok {
		t.Fatal("new hidden path missing after rename")
	}
	// delete
	if !f.hiddenDelete("/a/~$y.docx") {
		t.Fatal("hiddenDelete returned false")
	}
	if _, ok := f.hiddenGet("/a/~$y.docx"); ok {
		t.Fatal("hidden path still present after delete")
	}
}

func TestEmptyCreateFor(t *testing.T) {
	cases := []struct {
		name string
		want emptyCreateKind
	}{
		{"Textdokument (neu).txt", createText},
		{"notes.TXT", createText},
		{"index.html", createHTML},
		{"page.HTM", createHTML},
		{"report.docx", createFileInternal},
		{"bank.xlsx", createFileInternal},
		{"deck.pptx", createFileInternal},
		{"Bitmap-Bild (neu).bmp", createFileExternal},
		{"archive.zip", createFileExternal},
		{"noext", createFileExternal},
	}
	for _, c := range cases {
		if got := emptyCreateFor(c.name); got != c.want {
			t.Errorf("emptyCreateFor(%q) = %d, want %d", c.name, got, c.want)
		}
	}
}
