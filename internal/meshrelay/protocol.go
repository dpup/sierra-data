package meshrelay

import "time"

// Subprotocol names carry the protocol's major version.
const (
	BridgeSubprotocol = "sierra-mesh-bridge.v1"
	ClientSubprotocol = "sierra-mesh-client.v1"
	ProtocolVersion   = 1
)

// MaxFrameBytes is the largest binary message (one companion frame payload)
// either side may send. Current firmware's max frame is 176 bytes.
const MaxFrameBytes = 512

// maxControlBytes bounds a text (JSON control) message. Control messages are
// tiny; this only has to clear a hello with long names in it.
const maxControlBytes = 8 << 10

// Close codes. See the wire protocol's close-code table.
const (
	CloseNormal          = 1000
	CloseGoingAway       = 1001
	CloseBadToken        = 4001
	CloseGatewayMismatch = 4002
	CloseProtocolError   = 4003
	CloseUnsupported     = 4004
	CloseReplaced        = 4009
	CloseBusy            = 4029
)

// Node states as reported by the bridge.
const (
	NodeConnected    = "connected"
	NodeDisconnected = "disconnected"
	NodeMismatch     = "mismatch"
)

// lease_ending reasons. expiry, preempted and gateway_offline are the spec's;
// idle, revoked and pubkey_mismatch are additive values (receivers must treat an
// unknown reason as the lease ending).
const (
	ReasonExpiry         = "expiry"
	ReasonPreempted      = "preempted"
	ReasonGatewayOffline = "gateway_offline"
	ReasonIdle           = "idle"
	ReasonRevoked        = "revoked"
	ReasonPubkeyMismatch = "pubkey_mismatch"
	ReasonShutdown       = "relay_shutdown"
	// reasonClientClosed is audit-only: the holder hung up, nobody is told.
	reasonClientClosed = "client_closed"
)

// envelope is the common shape of every control message; Type routes it.
type envelope struct {
	Type string `json:"type"`
}

// Agent is hello.agent.
type Agent struct {
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
	OS      string `json:"os,omitempty"`
	Arch    string `json:"arch,omitempty"`
}

// Link is hello.link.
type Link struct {
	Kind   string `json:"kind,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// NodeInfo is hello.node and the body of node_status.
type NodeInfo struct {
	State  string `json:"state"`
	Pubkey string `json:"pubkey,omitempty"`
	Name   string `json:"name,omitempty"`
	FW     string `json:"fw,omitempty"`
	Model  string `json:"model,omitempty"`
	Error  string `json:"error,omitempty"`
}

type helloMsg struct {
	Type     string   `json:"type"`
	Protocol int      `json:"protocol"`
	Gateway  string   `json:"gateway"`
	Agent    Agent    `json:"agent"`
	Link     Link     `json:"link"`
	Node     NodeInfo `json:"node"`
}

type welcomeMsg struct {
	Type          string `json:"type"`
	Session       string `json:"session"`
	PingIntervalS int    `json:"ping_interval_s"`
	ServerTime    string `json:"server_time"`
}

// nodeStatusMsg carries the same fields as hello.node, flattened.
type nodeStatusMsg struct {
	Type string `json:"type"`
	NodeInfo
}

// Stats is the bridge's periodic stats message (minus the type).
type Stats struct {
	UptimeS            int64 `json:"uptime_s"`
	NodeLinkReconnects int64 `json:"node_link_reconnects"`
	FramesToNode       int64 `json:"frames_to_node"`
	FramesFromNode     int64 `json:"frames_from_node"`
	RelayReconnects    int64 `json:"relay_reconnects"`
}

type statsMsg struct {
	Type string `json:"type"`
	Stats
}

type attachMsg struct {
	Type   string `json:"type"`
	Lease  string `json:"lease"`
	Client string `json:"client"`
}

type detachMsg struct {
	Type  string `json:"type"`
	Lease string `json:"lease"`
}

type typeOnlyMsg struct {
	Type string `json:"type"`
}

type errorMsg struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

type leaseMsg struct {
	Type      string    `json:"type"`
	Lease     string    `json:"lease"`
	Gateway   string    `json:"gateway"`
	ExpiresAt string    `json:"expires_at"`
	Node      *NodeInfo `json:"node,omitempty"`
}

type leaseEndingMsg struct {
	Type   string `json:"type"`
	Lease  string `json:"lease"`
	InS    int    `json:"in_s"`
	Reason string `json:"reason"`
}

type renewMsg struct {
	Type  string `json:"type"`
	Lease string `json:"lease"`
}

type gatewayStatusMsg struct {
	Type   string    `json:"type"`
	Bridge string    `json:"bridge"`
	Node   *NodeInfo `json:"node,omitempty"`
}

func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }
