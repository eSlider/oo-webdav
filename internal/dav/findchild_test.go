package dav

import (
	"testing"

	"github.com/eslider/go-onlyoffice"
)

func TestFindChildTrimmed(t *testing.T) {
	l := &onlyoffice.DavListing{Folders: []onlyoffice.DavFolder{{ID: "1", Title: "Neuer Ordner."}}}
	// Exact name resolves.
	if n := findChild(l, "Neuer Ordner."); n == nil || n.id != "1" {
		t.Fatalf("exact match = %v, want id 1", n)
	}
	// Windows strips the trailing dot; the stripped name must still resolve.
	if n := findChild(l, "Neuer Ordner"); n == nil || n.id != "1" {
		t.Fatalf("trimmed match = %v, want id 1", n)
	}
	// Unrelated name stays unresolved.
	if n := findChild(l, "Anderer"); n != nil {
		t.Fatalf("unexpected match: %v", n)
	}
	// Ambiguous trailing-dot/space variants are not guessed.
	l2 := &onlyoffice.DavListing{Folders: []onlyoffice.DavFolder{{ID: "1", Title: "x."}, {ID: "2", Title: "x "}}}
	if n := findChild(l2, "x"); n != nil {
		t.Fatalf("ambiguous match should be nil, got %v", n)
	}
	// Exact still wins when present alongside a trimmed sibling.
	l3 := &onlyoffice.DavListing{Folders: []onlyoffice.DavFolder{{ID: "1", Title: "x."}, {ID: "2", Title: "x"}}}
	if n := findChild(l3, "x"); n == nil || n.id != "2" {
		t.Fatalf("exact should win: %v", n)
	}
}
