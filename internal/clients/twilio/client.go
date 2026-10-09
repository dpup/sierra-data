// Package twilio is a minimal client for the two Twilio REST operations the
// burn-line caller needs: place a recorded call, and fetch its recording.
//
// It is deliberately hand-rolled rather than pulling in the Twilio SDK. The
// surface used here is three form-encoded POSTs and a media download; the SDK
// would add a large dependency tree to a service whose other nine upstreams are
// plain HTTP.
package twilio

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// maxRecording caps a downloaded recording. A burn-line message is under a
// minute of mono mp3 (~1 MB); this is defensive headroom.
const maxRecording = 32 << 20 // 32 MiB

// HTTPDoer interface for HTTP clients (for testability).
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client calls the Twilio REST API for one account.
type Client struct {
	httpClient HTTPDoer
	baseURL    string
	accountSID string
	authToken  string
}

// NewClient builds a client for an account.
func NewClient(accountSID, authToken string) *Client {
	return &Client{
		httpClient: &http.Client{Timeout: 60 * time.Second},
		baseURL:    "https://api.twilio.com",
		accountSID: accountSID,
		authToken:  authToken,
	}
}

// NewClientWithHTTPDoer builds a client with a custom doer + base URL (testing).
func NewClientWithHTTPDoer(baseURL, accountSID, authToken string, doer HTTPDoer) *Client {
	return &Client{httpClient: doer, baseURL: strings.TrimRight(baseURL, "/"),
		accountSID: accountSID, authToken: authToken}
}

// Call is the subset of a Twilio call resource this package reads.
type Call struct {
	SID    string `json:"sid"`
	Status string `json:"status"` // queued|ringing|in-progress|completed|failed|busy|no-answer|canceled
}

// Recording is the subset of a Twilio recording resource this package reads.
type Recording struct {
	SID      string `json:"sid"`
	Duration string `json:"duration"` // seconds, as a string
}

type recordingList struct {
	Recordings []Recording `json:"recordings"`
}

// CreateCall places a recorded outbound call. twimlURL is a TwiML document
// telling Twilio what to do once answered (for a listen-only recording, a
// <Record> or <Pause> bin).
func (c *Client) CreateCall(ctx context.Context, to, from, twimlURL string, timeout time.Duration) (*Call, error) {
	form := url.Values{}
	form.Set("To", to)
	form.Set("From", from)
	form.Set("Url", twimlURL)
	form.Set("Record", "true")
	form.Set("RecordingChannels", "mono")
	form.Set("Timeout", fmt.Sprintf("%d", int(timeout.Seconds())))

	var call Call
	if err := c.do(ctx, http.MethodPost, "/Calls.json", form, &call); err != nil {
		return nil, fmt.Errorf("twilio: create call: %w", err)
	}
	return &call, nil
}

// GetCall fetches a call's current state.
func (c *Client) GetCall(ctx context.Context, sid string) (*Call, error) {
	var call Call
	if err := c.do(ctx, http.MethodGet, "/Calls/"+url.PathEscape(sid)+".json", nil, &call); err != nil {
		return nil, fmt.Errorf("twilio: get call %s: %w", sid, err)
	}
	return &call, nil
}

// EndCall hangs up an in-progress call. The burn line is a looping recorded
// message that never hangs up on its own, so the caller must.
func (c *Client) EndCall(ctx context.Context, sid string) error {
	form := url.Values{}
	form.Set("Status", "completed")
	if err := c.do(ctx, http.MethodPost, "/Calls/"+url.PathEscape(sid)+".json", form, nil); err != nil {
		return fmt.Errorf("twilio: end call %s: %w", sid, err)
	}
	return nil
}

// Recordings lists the recordings produced by a call.
func (c *Client) Recordings(ctx context.Context, callSID string) ([]Recording, error) {
	var list recordingList
	path := "/Recordings.json?CallSid=" + url.QueryEscape(callSID)
	if err := c.do(ctx, http.MethodGet, path, nil, &list); err != nil {
		return nil, fmt.Errorf("twilio: list recordings for %s: %w", callSID, err)
	}
	return list.Recordings, nil
}

// DownloadRecording fetches a recording as mp3 bytes.
func (c *Client) DownloadRecording(ctx context.Context, recordingSID string) ([]byte, error) {
	u := fmt.Sprintf("%s/2010-04-01/Accounts/%s/Recordings/%s.mp3",
		c.baseURL, url.PathEscape(c.accountSID), url.PathEscape(recordingSID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("twilio: build download request: %w", err)
	}
	req.SetBasicAuth(c.accountSID, c.authToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("twilio: download recording: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("twilio: download recording %s: status %d", recordingSID, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxRecording))
	if err != nil {
		return nil, fmt.Errorf("twilio: read recording: %w", err)
	}
	return b, nil
}

// do issues an authenticated request against the account's REST namespace and
// decodes the JSON response into out (which may be nil).
func (c *Client) do(ctx context.Context, method, path string, form url.Values, out any) error {
	u := fmt.Sprintf("%s/2010-04-01/Accounts/%s%s", c.baseURL, url.PathEscape(c.accountSID), path)

	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.SetBasicAuth(c.accountSID, c.authToken)
	req.Header.Set("Accept", "application/json")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// Twilio puts a human-readable reason in the body; carry it, since these
		// failures (unverified caller id, insufficient balance) are operational.
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(payload)))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return nil
}
