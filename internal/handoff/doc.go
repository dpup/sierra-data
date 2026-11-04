// Package handoff moves exclusive ownership of a SQLite database from one
// process to another without the old process having to exit first.
//
// The Grid and the mesh relay each hold an exclusive lock on their own SQLite
// file (grid.db, relay.db). A rolling deploy used to be stop-then-start: the
// replacement could not open the database until the old task had been
// drained, signalled, torn down and deprovisioned, and all of that was
// downtime. With handoff, a deployment coordinator provisions the replacement
// in STANDBY (config loaded, nothing opened), tells the old process to
// QUIESCE (stop admitting work, stop every database user, close the database,
// release the lock), and only then tells the replacement to ACTIVATE.
//
//	STANDBY ──activate──▶ ACTIVATING ──ok──▶ ACTIVE ──quiesce──▶ QUIESCING ──ok──▶ QUIESCED
//	   │                      │ failed                              │ failed
//	   │                      ▼                                     ▼
//	   │                   STANDBY                          QUIESCING (database still held)
//	   └──────────────quiesce (fence: never opened)───────────────────────────────▶ QUIESCED
//
// Rules the package enforces, because each one is a way to end up with two
// writers:
//
//   - Ownership is never inferred. Nothing here activates because a
//     predecessor timed out, went unhealthy or stopped answering. Activation
//     happens at boot (no --standby: the legacy path) or on an explicit,
//     authenticated POST /activate from the coordinator, which is responsible
//     for having confirmed the previous owner's release or termination. The
//     file lock the store takes is a secondary safeguard, not the protocol.
//   - QUIESCED is terminal. A released process never reopens the database;
//     a rollback is another handoff, in the other direction, to a process that
//     is in STANDBY.
//   - A failed activation releases everything it opened and returns to
//     STANDBY. A failed quiescence stays QUIESCING with the database counted as
//     HELD, so the coordinator sees an ambiguous handoff and must terminate the
//     old task before activating anything.
//   - SIGTERM in any state runs the same quiescence before the process exits.
//
// The control API listens on its own port (8081 Grid, 8091 relay), which
// must never be routed from the public load balancer. GET /status, /livez and
// /readyz are open; POST /activate and /quiesce need the deployment token as
// a bearer credential, checked against its SHA-256 (only the hash is in
// config). See docs/deploy-handoff.md for the coordinator's side.
package handoff
