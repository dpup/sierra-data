package twilio

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type routeDoer struct {
	// routes maps a substring of the request URL to a canned body.
	routes map[string]string
	status map[string]int
	seen   []*http.Request
	bodies []string
}

func (d *routeDoer) Do(req *http.Request) (*http.Response, error) {
	d.seen = append(d.seen, req)
	body := ""
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		body = string(b)
	}
	d.bodies = append(d.bodies, body)

	for frag, payload := range d.routes {
		if strings.Contains(req.URL.String(), frag) {
			code := 200
			if c, ok := d.status[frag]; ok {
				code = c
			}
			return &http.Response{
				StatusCode: code,
				Body:       io.NopCloser(strings.NewReader(payload)),
				Header:     make(http.Header),
			}, nil
		}
	}
	return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(`not found`)), Header: make(http.Header)}, nil
}

func newTestClient(d *routeDoer) *Client {
	return NewClientWithHTTPDoer("https://twilio.test", "AC123", "tok", d)
}

func TestCreateCall(t *testing.T) {
	d := &routeDoer{routes: map[string]string{
		"/Calls.json": `{"sid":"CA999","status":"queued"}`,
	}}
	call, err := newTestClient(d).CreateCall(context.Background(),
		"+12097546600", "+15550000000", "https://handler.example/twiml", 2*time.Minute)
	require.NoError(t, err)
	assert.Equal(t, "CA999", call.SID)
	assert.Equal(t, "queued", call.Status)

	// Recording must be requested, or there is nothing to transcribe.
	form, err := url.ParseQuery(d.bodies[0])
	require.NoError(t, err)
	assert.Equal(t, "+12097546600", form.Get("To"))
	assert.Equal(t, "+15550000000", form.Get("From"))
	assert.Equal(t, "true", form.Get("Record"))
	assert.Equal(t, "mono", form.Get("RecordingChannels"))
	assert.Equal(t, "120", form.Get("Timeout"))

	// Credentials go as basic auth, never in the URL.
	user, pass, ok := d.seen[0].BasicAuth()
	assert.True(t, ok)
	assert.Equal(t, "AC123", user)
	assert.Equal(t, "tok", pass)
	assert.NotContains(t, d.seen[0].URL.String(), "tok")
}

func TestEndCall(t *testing.T) {
	d := &routeDoer{routes: map[string]string{"/Calls/CA999.json": `{"sid":"CA999","status":"completed"}`}}
	require.NoError(t, newTestClient(d).EndCall(context.Background(), "CA999"))
	form, err := url.ParseQuery(d.bodies[0])
	require.NoError(t, err)
	assert.Equal(t, "completed", form.Get("Status"))
}

func TestRecordingsAndDownload(t *testing.T) {
	d := &routeDoer{routes: map[string]string{
		"/Recordings.json":    `{"recordings":[{"sid":"RE1","duration":"47"}]}`,
		"/Recordings/RE1.mp3": "ID3-fake-audio",
	}}
	c := newTestClient(d)

	recs, err := c.Recordings(context.Background(), "CA999")
	require.NoError(t, err)
	require.Len(t, recs, 1)
	assert.Equal(t, "RE1", recs[0].SID)

	audio, err := c.DownloadRecording(context.Background(), "RE1")
	require.NoError(t, err)
	assert.Equal(t, "ID3-fake-audio", string(audio))
}

// Twilio's operational failures (unverified caller id, no balance) come back as
// a 4xx with a human-readable body. That body must survive into the error, or
// the CI log says only "status 400".
func TestErrorCarriesTwilioMessage(t *testing.T) {
	d := &routeDoer{
		routes: map[string]string{"/Calls.json": `{"message":"The From number is not a valid, SMS-capable number"}`},
		status: map[string]int{"/Calls.json": 400},
	}
	_, err := newTestClient(d).CreateCall(context.Background(), "+1", "+2", "u", time.Minute)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a valid")
}
