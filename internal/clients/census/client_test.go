package census

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeDoer struct {
	status  int
	resp    string
	lastURL string
}

func (f *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	f.lastURL = req.URL.String()
	status := f.status
	if status == 0 {
		status = 200
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(f.resp)),
		Header:     make(http.Header),
	}, nil
}

const matchResponse = `{
  "result": {
    "input": {
      "address": {"address": "1201 Oak St, Arnold, CA"},
      "benchmark": {"benchmarkName": "Public_AR_Current"}
    },
    "addressMatches": [
      {
        "matchedAddress": "1201 OAK ST, ARNOLD, CA, 95223",
        "coordinates": {"x": -120.35213, "y": 38.25541},
        "tigerLine": {"tigerLineId": "123456", "side": "L"}
      },
      {
        "matchedAddress": "1201 OAK CT, ARNOLD, CA, 95223",
        "coordinates": {"x": -120.36, "y": 38.26}
      }
    ]
  }
}`

const noMatchResponse = `{"result": {"addressMatches": []}}`

func TestGeocodeMatch(t *testing.T) {
	doer := &fakeDoer{resp: matchResponse}
	c := NewClientWithHTTPDoer("https://census.test", doer)

	lat, lng, matched, err := c.Geocode(context.Background(), "1201 Oak St, Arnold, CA")
	require.NoError(t, err)
	assert.Equal(t, 38.25541, lat)
	assert.Equal(t, -120.35213, lng)
	assert.Equal(t, "1201 OAK ST, ARNOLD, CA, 95223", matched)

	// Query carries the address, benchmark, and format params.
	assert.Contains(t, doer.lastURL, "https://census.test/geocoder/locations/onelineaddress?")
	assert.Contains(t, doer.lastURL, "address=1201+Oak+St%2C+Arnold%2C+CA")
	assert.Contains(t, doer.lastURL, "benchmark=Public_AR_Current")
	assert.Contains(t, doer.lastURL, "format=json")
}

func TestGeocodeNoMatch(t *testing.T) {
	c := NewClientWithHTTPDoer("https://census.test", &fakeDoer{resp: noMatchResponse})

	_, _, _, err := c.Geocode(context.Background(), "nowhere at all")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNoMatch), "expected ErrNoMatch, got %v", err)
}

func TestGeocodeHTTPError(t *testing.T) {
	c := NewClientWithHTTPDoer("https://census.test", &fakeDoer{status: 500, resp: "internal error"})

	_, _, _, err := c.Geocode(context.Background(), "1201 Oak St, Arnold, CA")
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrNoMatch))
	assert.Contains(t, err.Error(), "500")
}

func TestGeocodeMalformedJSON(t *testing.T) {
	c := NewClientWithHTTPDoer("https://census.test", &fakeDoer{resp: "<html>not json</html>"})

	_, _, _, err := c.Geocode(context.Background(), "1201 Oak St, Arnold, CA")
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrNoMatch))
	assert.Contains(t, err.Error(), "decode")
}
