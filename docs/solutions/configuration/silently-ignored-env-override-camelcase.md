---
title: A misnamed PF__ env override is silently ignored, so the config file's value quietly wins
date: 2026-08-04
category: configuration
module: internal/config (prefab config loading)
problem_type: configuration_error
component: configuration
symptoms:
  - "PF__GRID__DBPATH set, but the event store still opens prefab.yaml's relative ./data/grid.db"
  - "An env override appears to have no effect, with no warning and no error"
  - "The dev sandbox's DB lands on the /workspace bind mount despite moat.yaml pointing elsewhere"
  - "A container's SQLite history disappears on every task replacement even though a volume is mounted"
root_cause: configuration_error
resolution_type: code_fix
severity: high
tags: [config, koanf, prefab, environment-variables, camelcase, sqlite, data-loss, fail-loud]
---

# A misnamed PF__ env override is silently ignored, so the config file's value quietly wins

## Problem

`PF__GRID__DBPATH=/data/grid.db` does nothing. The server starts, logs normally,
reports every source healthy — and opens the database at prefab.yaml's relative
`./data/grid.db` instead.

Prefab maps environment variables to config keys as `PF__A__B_C` → `a.bC`: `__`
separates path segments, and an `_` **inside** a segment produces the next
capital. So:

| env var | resolves to | matches `grid.dbPath`? |
|---|---|---|
| `PF__GRID__DB_PATH` | `grid.dbPath` | yes |
| `PF__GRID__DBPATH` | `grid.dbpath` | **no** |

`grid.dbpath` matches no struct field. koanf stores it, nothing reads it, and
nothing complains. Snake-case keys (`PF__SERVER__PORT` → `server.port`) are
unaffected, which is why the trap only springs on camelCase keys —
`grid.dbPath`, `grid.journalMode`, `grid.wildfire.placeBufferMeters`.

The consequences are silent and expensive:

- **Production**: the store lands at the image's `./data/grid.db` rather than the
  mounted `/data` volume, so the **entire revision history is discarded on every
  container replacement**. `internal/store/CLAUDE.md` is explicit that this data
  is irreplaceable — most upstreams are active-only, so a lost DB cannot be
  rebuilt beyond the current snapshot.
- **Dev sandbox**: `moat.yaml` set `PF__GRID__DBPATH` specifically to keep the DB
  **off** the `/workspace` bind mount, because virtiofs does not honour SQLite's
  locking and two writers corrupt the file. It was landing on the bind mount
  anyway — the exact hazard the setting existed to avoid.

Both present as "it works", right up until the data is gone.

## Root cause

Two things had to line up.

**The mapping is easy to get wrong**, and reads fine either way — `DBPATH` looks
more like a constant than `DB_PATH` does.

**The safety net was switched off for the keys that needed it.** Prefab *does*
validate unknown config keys, but `cmd/server` registers our namespaces as opaque
objects to stop it warning about every one of our own keys:

```go
// cmd/server/main.go — registerAppConfigKeys
for _, ns := range []string{"grid", "roads", "weather", "hazards", ...} {
    prefab.RegisterConfigKey(prefab.ConfigKeyInfo{Key: ns, Type: "object"})
}
```

Registering the namespace silences validation for **everything inside it**. So
prefab's own `server.*` keys stayed validated while ours — the camelCase-heavy
ones — did not. The Dockerfile had already been bitten and carried a comment
warning about the exact spelling; the root `CLAUDE.md`, `prefab.yaml`,
`moat.yaml`, `docs/`, and a store error message all still had the wrong name.
A comment in one file does not protect the other eight.

## Solution

Documenting the rule was the wrong fix — the whole failure mode is that nobody
reads a naming rule while they are typing an env var, and nothing tells them they
got it wrong. **Make it fail loudly instead**, and make it self-maintaining.

`internal/config/envcheck.go` reflects over the `Config` struct, resolves every
`PF__` variable that targets one of our namespaces, and refuses to start if it
maps to no real key:

```go
if err := ValidateEnvOverrides(os.Environ()); err != nil {
    log.Fatalf("Invalid configuration: %v", err)  // first thing LoadConfig does
}
```

```
Invalid configuration: silently-ignored environment override(s):
  - PF__GRID__DBPATH (resolves to config key "grid.dbpath", which does not exist)
    — did you mean PF__GRID__DB_PATH, for key "grid.dbPath"?
A camelCase config key needs an underscore in its env var (PF__A__B_C -> a.bC),
e.g. grid.dbPath is PF__GRID__DB_PATH
```

Design choices worth keeping:

- **Reflection over a registry.** Valid keys come from the struct's `koanf` tags,
  so a new field is covered the moment it exists. A hand-maintained list would
  drift, which is how we got here.
- **Case-insensitive re-resolve produces the suggestion.** The entire bug class
  is a casing mismatch, so when the exact lookup fails, retrying with
  `EqualFold` finds what the operator meant, and the canonical path is turned
  back into the correct env var name. An error that only says "unknown key"
  leaves them guessing.
- **Only our namespaces.** The first segment must match a top-level `Config`
  field, so prefab keeps ownership of `server.*` and there are no false positives
  from framework keys.
- **Maps validated to the leaf, slices skipped.** `grid.sources.<id>.<field>`
  consumes the map key and keeps checking (so `PF__GRID__SOURCES__USGS__POLLINTERVAL`
  is caught), while slice-valued config is accepted wholesale because koanf cannot
  merge an env key onto an array element anyway.
- **Fatal, not a warning.** A warning in a startup log is the same as silence.
  The failure it prevents is data loss, and the fix is a one-character edit.
- **Report every offender at once**, so fixing them is one pass rather than a
  game of whack-a-mole across restarts.

The wrong spellings were also corrected everywhere they appeared: `moat.yaml`
(the live one), root `CLAUDE.md`, `prefab.yaml`, `internal/config/config.go`,
`internal/store/store.go`'s error message, `internal/store/CLAUDE.md`, and two
`docs/` files — plus `PF__GRID__JOURNALMODE` → `PF__GRID__JOURNAL_MODE`, the same
defect. Historical `CHANGELOG.md` entries were left alone as dated records.

`envToConfigKey` replicates prefab's `TransformEnv`, which lives in prefab's
`internal/` package and cannot be imported; `TestEnvToConfigKey` pins the examples
from prefab's own doc comment so a drift fails the build rather than reopening
this bug.

## Prevention

- **When a framework offers key validation and you opt out of it, you owe a
  replacement.** Registering a namespace as opaque is a reasonable way to quiet
  the noise, but it silently un-guards everything beneath — and the keys you own
  are exactly the ones no one else is checking.
- **Treat "config that silently does nothing" as a fail-loud problem**, in the
  same family as a failed fetch becoming an all-clear. An override that is read
  but ignored is worse than one that errors, because it looks like it worked.
- **Verify an env override actually took effect the first time you use it** — the
  cheapest check is running with the var set to a scratch path and confirming the
  file appears. This bug was found exactly that way: a scratch DB that was never
  created.
- **A warning comment fixes one file.** If a footgun is worth commenting, it is
  usually worth a test or a startup check.

## Related

- `../../../internal/store/CLAUDE.md` — why the grid DB is a system of record and
  not a rehydrate-able cache (what losing it actually costs), and the
  bind-mount/virtiofs corruption hazard.
- `Dockerfile` — the original, correct `PF__GRID__DB_PATH` and the comment that
  first documented the trap.
