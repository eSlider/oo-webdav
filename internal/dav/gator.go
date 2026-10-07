package dav

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// gatorNamespace is the XML namespace of gator document fields exposed as
// WebDAV dead properties (see readFile.DeadProps).
const gatorNamespace = "https://produktor.io/gator#"

// gatorProps is the gator field set exposed per file.
type gatorProps struct {
	DocSHA    string
	Number    string
	Seller    string
	Total     string
	Typ       string
	IssuedAt  string
	CreatedAt string
	Status    string
	URL       string
}

// gatorInvoiceItem is the subset of GET /api/v1/invoices decoded per row.
type gatorInvoiceItem struct {
	DocSHA   string   `json:"doc_sha"`
	Number   string   `json:"number"`
	Seller   string   `json:"seller"`
	Total    *float64 `json:"total"`
	Date     string   `json:"date"`
	Type     string   `json:"type"`
	CanonTyp string   `json:"canon_typ"`
	Status   string   `json:"status"`
	OOFileID *string  `json:"oo_file_id"`
	OOURL    *string  `json:"oo_url"`
	Origin   struct {
		ReceivedAt string `json:"received_at"`
		IngestedAt string `json:"ingested_at"`
	} `json:"origin"`
}

// gatorIndex maps ONLYOFFICE file ids to gator fields. It is rebuilt in the
// background from GET /api/v1/invoices and swapped atomically, so PROPFIND is
// answered from memory and never calls gator on the hot path. A nil index is
// the disabled state (no GATOR_URL).
type gatorIndex struct {
	baseURL  string
	interval time.Duration
	client   *http.Client

	mu sync.RWMutex
	m  map[string]gatorProps
}

// newGatorIndex returns nil when baseURL is empty (feature off).
func newGatorIndex(baseURL string, interval time.Duration) *gatorIndex {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil
	}
	if interval <= 0 {
		interval = 15 * time.Minute
	}
	return &gatorIndex{
		baseURL:  baseURL,
		interval: interval,
		client:   &http.Client{Timeout: 60 * time.Second},
		m:        make(map[string]gatorProps),
	}
}

func (g *gatorIndex) lookup(fileID string) (gatorProps, bool) {
	if g == nil || fileID == "" {
		return gatorProps{}, false
	}
	g.mu.RLock()
	p, ok := g.m[fileID]
	g.mu.RUnlock()
	return p, ok
}

// run refreshes once, then on the configured interval, until ctx is done.
func (g *gatorIndex) run(ctx context.Context) {
	if g == nil {
		return
	}
	g.refresh(ctx)
	t := time.NewTicker(g.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			g.refresh(ctx)
		}
	}
}

func (g *gatorIndex) refresh(ctx context.Context) {
	next := make(map[string]gatorProps)
	const pageSize = 200
	offset := 0
	for {
		items, total, err := g.page(ctx, offset, pageSize)
		if err != nil {
			// Keep the previous snapshot: a gator outage must not blank the
			// properties for every file.
			log.Printf("gator: refresh failed at offset %d: %v", offset, err)
			return
		}
		for _, it := range items {
			if it.OOFileID == nil || *it.OOFileID == "" {
				continue
			}
			typ := it.CanonTyp
			if typ == "" {
				typ = it.Type
			}
			created := it.Origin.IngestedAt
			if created == "" {
				created = it.Origin.ReceivedAt
			}
			next[*it.OOFileID] = gatorProps{
				DocSHA:    it.DocSHA,
				Number:    it.Number,
				Seller:    it.Seller,
				Total:     formatAmount(it.Total),
				Typ:       typ,
				IssuedAt:  it.Date,
				CreatedAt: created,
				Status:    it.Status,
				URL:       strPtr(it.OOURL),
			}
		}
		offset += len(items)
		if len(items) == 0 || offset >= total {
			break
		}
	}
	g.mu.Lock()
	g.m = next
	g.mu.Unlock()
	log.Printf("gator: indexed %d files with oo_file_id", len(next))
}

func (g *gatorIndex) page(ctx context.Context, offset, limit int) ([]gatorInvoiceItem, int, error) {
	u := fmt.Sprintf("%s/api/v1/invoices?limit=%d&offset=%d", g.baseURL, limit, offset)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("gator %s: HTTP %d", u, resp.StatusCode)
	}
	var env struct {
		Items  []gatorInvoiceItem `json:"items"`
		Total  int                `json:"total"`
		Limit  int                `json:"limit"`
		Offset int                `json:"offset"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, 0, err
	}
	return env.Items, env.Total, nil
}

func formatAmount(v *float64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatFloat(*v, 'f', 2, 64)
}

func strPtr(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

// gatorPropNames is the property name set served by DeadProps, in XML order.
var gatorPropNames = []string{
	"doc_sha", "rechnungsnummer", "lieferant", "betrag", "sum",
	"typ", "issued_at", "created_at", "status", "url",
}

// gatorPropValues maps the ordered names to values for a file.
func gatorPropValues(p gatorProps) map[string]string {
	return map[string]string{
		"doc_sha":         p.DocSHA,
		"rechnungsnummer": p.Number,
		"lieferant":       p.Seller,
		"betrag":          p.Total,
		"sum":             p.Total,
		"typ":             p.Typ,
		"issued_at":       p.IssuedAt,
		"created_at":      p.CreatedAt,
		"status":          p.Status,
		"url":             p.URL,
	}
}
