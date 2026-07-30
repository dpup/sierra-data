# Concepts

Shared domain vocabulary for this project — entities, named processes, and status concepts with project-specific meaning. Seeded with core domain vocabulary, then accretes as ce-compound and ce-compound-refresh process learnings; direct edits are fine. Glossary only, not a spec or catch-all.

## Events & lifecycle

### Event
A single occurrence the Grid tracks and serves — a wildfire, an evacuation, a weather alert, an earthquake, a road incident, a mesh node — as a canonical record carrying geometry, severity, a source-namespaced id, and full revision history.

An Event has a lifecycle status: Active or Scheduled while live, Resolved when a feed that authoritatively lists everything current drops it, Expired when a feed that goes quiet without confirming an ending passes the event's own expiry or a per-source grace. Every status change is a Revision, so the record of an ending is part of history, not a deletion.

### Revision
An immutable snapshot of an Event written on any content change or lifecycle transition. Volatile bookkeeping (re-stamped timestamps, AI enhancement, high-frequency telemetry) is excluded from the change test, so re-observing an unchanged record does not mint a Revision.

### Disappearance sweep
The per-tick reconciliation that transitions Events which are absent from a *successful, complete* poll of their Source, governed by that Source's disappearance policy (resolve immediately, or expire only after a grace). See Fail-loud.

### Fail-loud
The invariant that a failed or incomputable fetch must never resolve or expire Events — an error must never become an all-clear. Absence is meaningful only against a successful, complete poll; a failed or partial poll suppresses the sweep for the affected Source rather than treating missing data as gone.

## Ingest

### Normalizer
The ingest unit that fetches one scope of upstream feeds each tick and maps them into canonical Events; its runtime instance is a poller (one per scope, ticking on its own interval).
*Avoid:* using "source" for this — a Normalizer is not a Source.

A Normalizer may write health for several Sources (a poller ≠ a Source): the wildfire poller joins a fire-incident feed with a perimeter feed and reports health for both.

### Source
A registered upstream feed, carrying its own health (healthy, stale, or unavailable, degrading as polls fail) and its disappearance policy. Distinct from the poller that reads it — one poller can update several Source rows.

## Wildfire perimeters

### Perimeter
A fire's mapped extent polygon obtained from an upstream perimeter feed. A Perimeter is either adopted onto an incident Event or emitted standalone (see Adoption, Standalone perimeter). An upstream feed may carry many rows for one fire (successive aerial-mapping flights); these are deduplicated to one Perimeter per fire before adoption.

### Adoption
Attaching a Perimeter polygon to a fire-incident Event by an unambiguous normalized-name match, so the incident renders as its true extent rather than a point. An ambiguous name (two distinct fires normalizing alike) is never adopted — both go standalone.

### Standalone perimeter
A Perimeter that no incident adopted (a mapped fire the curated incident feed omits), emitted as its own Event under a perimeter-source-namespaced id. It carries the perimeter's geometry but not incident-only fields like containment.
