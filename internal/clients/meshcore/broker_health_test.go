package meshcore

import (
	"strings"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stateClient is an mqtt.Client whose connection state is set by the test.
// Embedding the interface leaves every other method nil: only the two state
// queries are ever called by Brokers.
type stateClient struct {
	mqtt.Client
	open, connected bool
}

func (c *stateClient) IsConnectionOpen() bool { return c.open }
func (c *stateClient) IsConnected() bool      { return c.connected }

// newLinkedRegistry wires one broker link to a fake client, the shape Connect
// leaves behind, without dialing anything.
func newLinkedRegistry(t *testing.T, client mqtt.Client, startedAt, now time.Time, silence time.Duration) (*Registry, *brokerLink) {
	t.Helper()
	r := NewRegistry(Config{SilenceAfter: silence})
	r.now = func() time.Time { return now }
	link := &brokerLink{url: "wss://mqtt.example:443/mqtt", startedAt: startedAt}
	r.clients = []mqtt.Client{client}
	r.links = []*brokerLink{link}
	return r, link
}

// The trap this exists for: with auto-reconnect on, paho's IsConnected stays
// true while the client is reconnecting, so a broker that had dropped us read
// as connected for as long as it stayed gone. Open must be the real session.
func TestBrokersIgnoresPahoReconnectingAsConnected(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	reconnecting := &stateClient{open: false, connected: true}
	r, link := newLinkedRegistry(t, reconnecting, now.Add(-time.Hour), now, 15*time.Minute)
	link.lastMsg.Store(now.Add(-5 * time.Hour).UnixNano())

	got := r.Brokers()
	require.Len(t, got, 1)
	assert.False(t, got[0].Open, "a reconnecting client has no session")
	assert.False(t, got[0].Delivering())
	assert.Equal(t, "not connected (last message 5h0m0s ago)", got[0].Problem)
	assert.Equal(t, now.Add(-5*time.Hour), got[0].LastMsgAt.UTC())
}

func TestBrokersReportsAnOpenSilentSession(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	r, link := newLinkedRegistry(t, &stateClient{open: true, connected: true}, now.Add(-time.Hour), now, 15*time.Minute)
	link.lastMsg.Store(now.Add(-20 * time.Minute).UnixNano())

	got := r.Brokers()
	require.Len(t, got, 1)
	assert.True(t, got[0].Open)
	assert.Equal(t, "connected but silent (last message 20m0s ago)", got[0].Problem)
}

// Every message counts, whatever its packet type: the question is whether the
// pipe carries anything, and on the global topic it carries several a second.
func TestOnMessageStampsTheDeliveryClock(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	r, link := newLinkedRegistry(t, &stateClient{open: true, connected: true}, now.Add(-time.Hour), now, 15*time.Minute)

	require.False(t, r.Brokers()[0].Delivering(), "an hour with no message is silence")

	// A TXT_MSG envelope (packet_type 2): ignored as data, but proof of delivery.
	r.onMessage(link)(nil, fakeMessage(`{"packet_type":"2","raw":"0900"}`))

	got := r.Brokers()[0]
	assert.True(t, got.Delivering())
	assert.Equal(t, now, got.LastMsgAt.UTC())
	assert.Empty(t, r.Snapshot(), "a non-advert updates the clock, not the node registry")
}

func TestDeliveryProblem(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	started := now.Add(-time.Hour)
	const window = 15 * time.Minute

	cases := []struct {
		name    string
		open    bool
		started time.Time
		lastMsg time.Time
		silence time.Duration
		want    string // "" = delivering; otherwise a prefix of the problem
	}{
		{name: "open and recent", open: true, started: started, lastMsg: now.Add(-time.Second), silence: window},
		{name: "open, exactly at the window", open: true, started: started, lastMsg: now.Add(-window), silence: window},
		{name: "open, past the window", open: true, started: started, lastMsg: now.Add(-window - time.Second), silence: window,
			want: "connected but silent (last message 15m1s ago)"},
		{name: "just started, nothing yet, inside the window", open: true, started: now.Add(-time.Minute), silence: window},
		{name: "never carried anything since start", open: true, started: started, silence: window,
			want: "connected but silent (no message since 2026-10-09T11:00:00Z)"},
		{name: "not connected beats a fresh clock", open: false, started: started, lastMsg: now.Add(-time.Second), silence: window,
			want: "not connected (last message 1s ago)"},
		{name: "not connected and never heard", open: false, started: started, silence: window,
			want: "not connected (no message since 2026-10-09T11:00:00Z)"},
		{name: "silence check disabled: open is enough", open: true, started: started, lastMsg: now.Add(-48 * time.Hour), silence: -1},
		{name: "silence check disabled still needs a session", open: false, started: started, silence: 0,
			want: "not connected"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := deliveryProblem(tc.open, tc.started, tc.lastMsg, now, tc.silence)
			if tc.want == "" {
				assert.Empty(t, got)
				return
			}
			assert.True(t, strings.HasPrefix(got, tc.want), "got %q, want prefix %q", got, tc.want)
		})
	}
}

// A reconnect must not make a broker that carries nothing look fresh. Silence is
// measured from the last message, else from when we first dialed — so a session
// that keeps being dropped and re-established stays silent.
func TestDeliveryProblemIgnoresReconnects(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	firstDialed := now.Add(-6 * time.Hour)
	// The session is open (it reconnected a moment ago), but nothing has arrived
	// since the first dial six hours back.
	assert.NotEmpty(t, deliveryProblem(true, firstDialed, time.Time{}, now, 15*time.Minute))
}

// Two processes on one client id make the broker close the older session each
// time the other connects. The suffix is per process, and stable within it so
// our own reconnects keep the id.
func TestClientIDIsUniquePerProcess(t *testing.T) {
	a, b := NewRegistry(Config{}), NewRegistry(Config{})
	named := Broker{URL: "wss://mqtt.example", ClientID: "data.sierragridteam.org"}

	idA := a.clientID(0, named)
	assert.True(t, strings.HasPrefix(idA, "data.sierragridteam.org-"), "the configured id still names us: %s", idA)
	assert.Len(t, strings.TrimPrefix(idA, "data.sierragridteam.org-"), 8)
	assert.Equal(t, idA, a.clientID(0, named), "stable for the life of the registry")
	assert.NotEqual(t, idA, b.clientID(0, named), "two registries never share a session")

	assert.True(t, strings.HasPrefix(a.clientID(1, Broker{URL: "wss://x"}), "sierra-grid-meshcore-1-"),
		"the default base keeps its broker index")
}

func TestBuildClientUsesTheUniqueID(t *testing.T) {
	r := NewRegistry(Config{})
	b := Broker{URL: "wss://mqtt.example:443/mqtt", ClientID: "data.sierragridteam.org"}
	c := r.buildClient(t.Context(), 0, b, &brokerLink{url: b.URL})
	opts := c.OptionsReader()
	assert.Equal(t, r.clientID(0, b), opts.ClientID())
}

// fakeMessage is the minimum mqtt.Message onMessage reads.
type fakeMessage string

func (m fakeMessage) Duplicate() bool   { return false }
func (m fakeMessage) Qos() byte         { return 0 }
func (m fakeMessage) Retained() bool    { return false }
func (m fakeMessage) Topic() string     { return "meshcore/OAK/ABCD/packets" }
func (m fakeMessage) MessageID() uint16 { return 0 }
func (m fakeMessage) Payload() []byte   { return []byte(m) }
func (m fakeMessage) Ack()              {}
