package dav

import (
	"encoding/xml"
	"testing"
)

func TestReadFileDeadProps(t *testing.T) {
	idx := &gatorIndex{m: map[string]gatorProps{
		"42": {
			DocSHA: "abc", Number: "RE-2026-1", Seller: "Müller & Co <GmbH>",
			Total: "123.45", Typ: "eingangsrechnung", IssuedAt: "2026-01-02",
			CreatedAt: "2026-01-03T04:05:06Z", Status: "ok", URL: "https://x/a?b=1&c=2",
		},
	}}
	r := &readFile{fs: &fs{gator: idx}, node: &node{id: "42"}}
	props, err := r.DeadProps()
	if err != nil {
		t.Fatalf("DeadProps: %v", err)
	}
	get := func(local string) string {
		return string(props[xml.Name{Space: gatorNamespace, Local: local}].InnerXML)
	}
	if got := get("rechnungsnummer"); got != "RE-2026-1" {
		t.Errorf("rechnungsnummer = %q", got)
	}
	if got := get("lieferant"); got != "Müller &amp; Co &lt;GmbH&gt;" {
		t.Errorf("lieferant = %q", got)
	}
	if got := get("betrag"); got != "123.45" {
		t.Errorf("betrag = %q", got)
	}
	if got := get("sum"); got != "123.45" {
		t.Errorf("sum = %q", got)
	}
	if got := get("created_at"); got != "2026-01-03T04:05:06Z" {
		t.Errorf("created_at = %q", got)
	}
	if got := get("url"); got != "https://x/a?b=1&amp;c=2" {
		t.Errorf("url = %q", got)
	}

	// File without a gator record → no properties.
	r2 := &readFile{fs: &fs{gator: idx}, node: &node{id: "99"}}
	if p, err := r2.DeadProps(); err != nil || len(p) != 0 {
		t.Errorf("unknown file: props=%d err=%v, want empty", len(p), err)
	}
	// Feature disabled (nil index) → no properties.
	r3 := &readFile{fs: &fs{}, node: &node{id: "42"}}
	if p, err := r3.DeadProps(); err != nil || len(p) != 0 {
		t.Errorf("nil index: props=%d err=%v, want empty", len(p), err)
	}
}
