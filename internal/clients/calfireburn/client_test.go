package calfireburn

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	_ "time/tzdata"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubDoer struct {
	status int
	body   string
	err    error
	seen   *http.Request
}

func (s *stubDoer) Do(req *http.Request) (*http.Response, error) {
	s.seen = req
	if s.err != nil {
		return nil, s.err
	}
	return &http.Response{
		StatusCode: s.status,
		Body:       io.NopCloser(strings.NewReader(s.body)),
		Header:     make(http.Header),
	}, nil
}

func fixture(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../../tests/testdata/calfireburn/current-burn-status.html")
	require.NoError(t, err)
	return string(b)
}

func TestGetBurnStatus_ParsesLiveFixture(t *testing.T) {
	d := &stubDoer{status: 200, body: fixture(t)}
	got, err := NewClientWithHTTPDoer("https://example.test", d).GetBurnStatus(context.Background())
	require.NoError(t, err)

	cal, ok := got["calaveras"]
	require.True(t, ok, "calaveras must be present; got keys %v", keys(got))
	assert.Equal(t, "Calaveras", cal.County)
	assert.Equal(t, StatusBurningSuspended, cal.Status)
	assert.Equal(t, "Burning Suspended", cal.RawStatus)
	assert.Equal(t, "All SRA", cal.Area)

	// "Effective, June 15, 2026 at 8:00 AM" is PACIFIC with no zone marker.
	// 8:00 PDT == 15:00 UTC. Parsing it as UTC would be off by 7 hours and every
	// suspension would appear to start in the middle of the night.
	assert.Equal(t, time.Date(2026, 6, 15, 15, 0, 0, 0, time.UTC), cal.Effective.UTC())

	_, ok = got["tuolumne"]
	assert.True(t, ok, "tuolumne must be present")
}

// The header row must not be mistaken for a county.
func TestGetBurnStatus_SkipsHeaderRow(t *testing.T) {
	d := &stubDoer{status: 200, body: fixture(t)}
	got, err := NewClientWithHTTPDoer("https://example.test", d).GetBurnStatus(context.Background())
	require.NoError(t, err)
	_, ok := got["county"]
	assert.False(t, ok, "the 'County' header must not parse as a county")
}

// The browser headers are load-bearing: the live host 403s any request that
// does not present as a browser navigation. Pin them so they cannot be
// "cleaned up" without the test failing.
func TestGetBurnStatus_SendsBrowserHeaders(t *testing.T) {
	d := &stubDoer{status: 200, body: fixture(t)}
	_, err := NewClientWithHTTPDoer("https://example.test", d).GetBurnStatus(context.Background())
	require.NoError(t, err)
	require.NotNil(t, d.seen)
	assert.Contains(t, d.seen.Header.Get("User-Agent"), "Chrome/")
	assert.Contains(t, d.seen.Header.Get("Accept"), "text/html")
	assert.Equal(t, "en-US,en;q=0.9", d.seen.Header.Get("Accept-Language"))
	assert.Equal(t, "document", d.seen.Header.Get("Sec-Fetch-Dest"))
	assert.Equal(t, "navigate", d.seen.Header.Get("Sec-Fetch-Mode"))
	assert.Equal(t, "none", d.seen.Header.Get("Sec-Fetch-Site"))
}

// A 403 is the EXPECTED failure when the edge's fingerprinting changes, so it
// must be a named error rather than a bare status code.
func TestGetBurnStatus_403IsNamed(t *testing.T) {
	d := &stubDoer{status: 403, body: "Access Denied"}
	_, err := NewClientWithHTTPDoer("https://example.test", d).GetBurnStatus(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "403")
	assert.Contains(t, err.Error(), "bot protection")
}

// THE important failure mode. If the markup moves and no rows parse, this MUST
// error. Returning an empty map would tell ingest that no county has a
// suspension — in fire season the most dangerous wrong answer this package
// could produce.
func TestGetBurnStatus_EmptyTableIsAnError(t *testing.T) {
	for name, body := range map[string]string{
		"no rows":     `<html><body><table id="datatable"></table></body></html>`,
		"no table":    `<html><body><p>We have moved this page.</p></body></html>`,
		"header only": `<html><body><table><tr><th>County</th><th>Status</th></tr></table></body></html>`,
	} {
		t.Run(name, func(t *testing.T) {
			d := &stubDoer{status: 200, body: body}
			got, err := NewClientWithHTTPDoer("https://example.test", d).GetBurnStatus(context.Background())
			require.Error(t, err, "must not silently report zero suspensions")
			assert.Nil(t, got)
		})
	}
}

func TestParseStatus(t *testing.T) {
	cases := map[string]Status{
		"Burning Suspended":  StatusBurningSuspended,
		"burning suspended":  StatusBurningSuspended,
		"Permit Required":    StatusPermitRequired,
		"No Permit Required": StatusNoPermitRequired,
		"Something New":      StatusUnknown, // unknown wording is never permissive
		"":                   StatusUnknown,
	}
	for in, want := range cases {
		assert.Equal(t, want, parseStatus(in), "parseStatus(%q)", in)
	}
}

func TestParseEffective(t *testing.T) {
	// 8:00 AM PDT -> 15:00 UTC
	assert.Equal(t, time.Date(2026, 6, 15, 15, 0, 0, 0, time.UTC),
		parseEffective("Effective, June 15, 2026 at 8:00 AM").UTC())
	// 5:00 PM PDT -> 00:00 UTC next day
	assert.Equal(t, time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC),
		parseEffective("Effective, June 29, 2026 at 5:00 PM").UTC())
	// Unparseable is the zero time, not a wrong time.
	assert.True(t, parseEffective("whenever").IsZero())
}

func keys(m map[string]CountyStatus) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
