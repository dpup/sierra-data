// Package meshrelay is the SIERRA Mesh Relay: the meeting point between
// MeshCore gateways at volunteers' houses and the authenticated clients that
// talk to a node through them.
//
//	node <--serial/TCP--> bridge ==WSS (bridge dials out)==> relay <==WSS== client
//
// A bridge dials OUT to the relay, so a house needs no open port and works
// behind CGNAT. The wire protocol is docs/mesh-relay-wireprotocol.md (shared
// verbatim with the bridge repo); the service requirements are
// docs/mesh-relay-server.md. Read both before changing anything here.
//
// # Why a separate process
//
// The relay runs as its own binary (cmd/mesh-relay) with its own SQLite file,
// and nothing in cmd/server imports this package. A relay crash must not touch
// the hazard feeds, and vice versa. Keep it that way: do not mount this plugin
// on the Grid's server.
//
// # Built on prefab
//
// It is still a prefab application, like the Grid: prefab owns the HTTP
// server, config (prefab.yaml's meshRelay section, PF__MESH_RELAY__*), logging
// and errors. The Relay is a prefab plugin — ServerOptions (OptionProvider)
// mounts its routes, Init starts its background loop, Shutdown closes the
// hijacked websockets that prefab's HTTP drain cannot see. The admin API uses
// prefab JSON handlers, so its errors are the {code, codeName, message}
// envelope of /api/v1. Websockets are plain handlers on prefab's mux; prefab's
// gzip wrapper passes http.Hijacker through.
//
// # Invariants
//
//   - No unauthenticated path reaches a node. Every route that forwards frames
//     authenticates a bearer token first. The only public routes are /healthz
//     and /api/status, a deliberately
//     minimal board for the public site (see apiPublicStatus for what it
//     withholds, and why).
//   - Payloads are never logged. Binary frames carry private messages and
//     channel secrets (CHANNEL_INFO returns the 16-byte key). Log sizes,
//     counts and timing only. The one exception is the per-gateway debug list
//     (Config.DebugPayloadGateways), which is off by default and announced
//     loudly at startup.
//   - Tokens are stored as SHA-256 hashes only, and shown once at creation.
//   - One lease per gateway: the companion protocol supports one app session
//     per node, so a lease is exclusive.
//
// # Shape
//
// Relay owns live state (bridge sessions, leases) under one mutex. The frame
// path never touches the database: audit and status writes go through an
// asynchronous writer (see Relay.audit), so a slow EFS round trip cannot stall
// forwarding. Security-relevant writes (pubkey pinning, token changes) are
// synchronous and happen outside the mutex.
//
// A lease holder is an interface (LeaseHolder), not a websocket, so the
// phase-3 internal poller can hold a lease exactly like an external client,
// at a lower Priority that a human session preempts.
package meshrelay
