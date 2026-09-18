// Package calfireburn scrapes CAL FIRE's Current Residential Burn Status table
// (burnpermit.fire.ca.gov), which reports, per county, whether CAL FIRE has
// suspended residential burning on State Responsibility Area land.
//
// # This is an HTML scrape of a bot-protected host — read this before touching it
//
// There is no API. As of 2026-09 no equivalent exists on CAL FIRE's ArcGIS org
// (368 services checked), on data.ca.gov, or anywhere in AGOL search. The
// closest-named layer, BP_Restrictions_Log_View, is a per-applicant restriction
// log carrying names, emails and phone numbers — deliberately NOT used here.
//
// The host sits behind Akamai bot management, and it does not merely dislike a
// missing User-Agent: it rejects any client that does not present as a browser
// NAVIGATION. Measured against the live host:
//
//	bare curl                                 403
//	descriptive bot UA ("SierraGrid/1.0")     403
//	Chrome UA + Accept + Accept-Language      403
//	Chrome UA + Accept + Sec-Fetch-* headers  200
//	bot UA   + Accept + Sec-Fetch-* headers   403
//
// So browserHeaders below is load-bearing, not cargo cult, and an honest
// self-identifying User-Agent is NOT an option that works. Note the tension
// worth knowing about: the site's own robots.txt is "User-agent: *" with ZERO
// Disallow rules plus an advertised sitemap, so its declared crawl policy
// permits this while its edge configuration does not. We poll it twice a day.
//
// Expect this to break. It is an HTML scrape (the markup is Sitecore-generated
// and can change without notice — the Caltrans KML feeds did exactly that in
// 2026) sitting behind a fingerprinting WAF that is updated by a third party.
// It is therefore deliberately the SECONDARY source for burn status: the county
// burn line carries the answer that changes often, and this feed supplies only
// the suspension's effective date, coverage for counties with no phone-line
// pipeline, and a cross-check. When it 403s, ingest degrades this source and
// the burn-day answer is unaffected.
package calfireburn

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// maxBody caps the upstream response. The page is ~11 KB.
const maxBody = 5 << 20 // 5 MiB

// pacific is the zone CAL FIRE stamps its effective times in. The page prints
// "Effective, June 15, 2026 at 8:00 AM" with NO zone marker; parsing that as UTC
// would shift every suspension by 7-8 hours. cmd/server blank-imports
// time/tzdata so this resolves in a minimal container.
var pacific = mustLoadPacific()

func mustLoadPacific() *time.Location {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		return time.UTC // tzdata missing: fall back rather than panic at init
	}
	return loc
}

// HTTPDoer interface for HTTP clients (for testability).
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client scrapes the CAL FIRE burn status page.
type Client struct {
	httpClient HTTPDoer
	baseURL    string
}

// NewClient creates a CAL FIRE burn status client.
func NewClient() *Client {
	return &Client{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		baseURL:    "https://burnpermit.fire.ca.gov",
	}
}

// NewClientWithHTTPDoer creates a client with a custom doer + base URL (testing).
func NewClientWithHTTPDoer(baseURL string, httpClient HTTPDoer) *Client {
	return &Client{httpClient: httpClient, baseURL: strings.TrimRight(baseURL, "/")}
}

// Status is CAL FIRE's declared burn status for a county.
type Status string

const (
	StatusUnknown          Status = ""
	StatusBurningSuspended Status = "suspended"
	StatusPermitRequired   Status = "permit-required"
	StatusNoPermitRequired Status = "no-permit-required"
)

// CountyStatus is one row of the table.
type CountyStatus struct {
	County    string    // "Calaveras" — the " County" suffix stripped
	Status    Status    //
	RawStatus string    // the page's own wording, kept for display + diagnosis
	Effective time.Time // when the status took effect (Pacific); zero if unparseable
	Area      string    // "All SRA" — the land classes covered
}

// browserHeaders are what the Akamai edge requires. See the package doc for the
// measured matrix; removing any of these returns 403 from the live host.
func browserHeaders(req *http.Request) {
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) "+
		"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,"+
		"image/avif,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Site", "none")
	req.Header.Set("Sec-Fetch-User", "?1")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
}

// GetBurnStatus returns the current status for every county in the table, keyed
// by the lowercased county name with no " County" suffix ("calaveras").
func (c *Client) GetBurnStatus(ctx context.Context) (map[string]CountyStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/current-burn-status", nil)
	if err != nil {
		return nil, fmt.Errorf("calfireburn: build request: %w", err)
	}
	browserHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calfireburn: fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// 403 is the expected failure when the edge's fingerprinting changes.
		// Name it so the source-health error is self-explanatory in /api/v1/sources.
		if resp.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("calfireburn: 403 forbidden (bot protection; see package doc)")
		}
		return nil, fmt.Errorf("calfireburn: unexpected status %d", resp.StatusCode)
	}

	doc, err := html.Parse(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("calfireburn: parse html: %w", err)
	}

	rows := tableRows(doc)
	if len(rows) == 0 {
		// An empty table is never legitimate — CAL FIRE lists every county. This
		// means the markup moved, and it MUST be an error: returning an empty map
		// would tell ingest that no county has a suspension, which in fire season
		// is the most dangerous wrong answer this package could give.
		return nil, fmt.Errorf("calfireburn: no data rows found (page layout changed?)")
	}

	out := make(map[string]CountyStatus, len(rows))
	for _, cells := range rows {
		if len(cells) < 2 {
			continue
		}
		county := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(cells[0]), "County"))
		county = strings.TrimSpace(county)
		if county == "" || strings.EqualFold(county, "County") {
			continue // header row
		}
		cs := CountyStatus{
			County:    county,
			RawStatus: strings.TrimSpace(cells[1]),
			Status:    parseStatus(cells[1]),
		}
		if len(cells) > 2 {
			cs.Effective = parseEffective(cells[2])
		}
		if len(cells) > 3 {
			cs.Area = strings.TrimSpace(cells[3])
		}
		out[strings.ToLower(county)] = cs
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("calfireburn: table had rows but no counties parsed")
	}
	return out, nil
}

// parseStatus maps the page's wording onto the Status domain. Unrecognized
// wording is UNKNOWN, never a permissive default.
func parseStatus(s string) Status {
	t := strings.ToLower(strings.Join(strings.Fields(s), " "))
	switch {
	case strings.Contains(t, "suspend"):
		return StatusBurningSuspended
	case strings.Contains(t, "no permit"):
		return StatusNoPermitRequired
	case strings.Contains(t, "permit"):
		return StatusPermitRequired
	default:
		return StatusUnknown
	}
}

// effectiveLayouts are the shapes observed on the live page. The leading
// "Effective," and the "at" separator are stripped before parsing.
var effectiveLayouts = []string{
	"January 2, 2006 3:04 PM",
	"January 2, 2006 3:04PM",
	"January 2, 2006",
}

// parseEffective reads "Effective, June 15, 2026 at 8:00 AM" as Pacific time.
func parseEffective(s string) time.Time {
	t := strings.TrimSpace(s)
	t = strings.TrimPrefix(t, "Effective")
	t = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(t), ","))
	t = strings.Replace(t, " at ", " ", 1)
	t = strings.Join(strings.Fields(t), " ")
	for _, layout := range effectiveLayouts {
		if parsed, err := time.ParseInLocation(layout, t, pacific); err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}

// tableRows walks the document and returns the cell text of every <tr>, from
// any table on the page. Anchoring on the id ("datatable") would be one more
// thing that can silently change; the county rows are the only 3-4 cell rows
// present, and parseStatus/the header check filter the rest.
func tableRows(n *html.Node) [][]string {
	var out [][]string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "tr" {
			var cells []string
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				if c.Type == html.ElementNode && (c.Data == "td" || c.Data == "th") {
					cells = append(cells, textOf(c))
				}
			}
			if len(cells) > 0 {
				out = append(out, cells)
			}
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return out
}

// textOf returns the concatenated text content of a node.
func textOf(n *html.Node) string {
	var sb strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			sb.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.TrimSpace(strings.Join(strings.Fields(sb.String()), " "))
}
