#!/usr/bin/env bash
# Gate 3 of the PostgreSQL migration plan: a local blue/green rehearsal of the
# writer lease (docs/deploy-handoff.md, "Rehearsal").
#
#   make rehearse-bluegreen      # builds bin/server, starts the Docker database
#
# On a throwaway database (dropped and recreated) it runs:
#   1. server A at boot: it takes the writer lease;
#   2. server B at boot: a reader that campaigns; it must serve reads and
#      forward a push to A;
#   3. steady load on B (reads, and pushes with the client retry a reporter
#      uses: on 503, wait Retry-After and try again), then SIGTERM to A;
#      B must take the lease within a few seconds;
#   4. A again (now the reader), steady load on A, then SIGKILL to B (a crash:
#      no release); A must take over once the dead session's lock is gone;
#   5. the lease epochs must have moved 1 -> 2 -> 3, one holder at a time.
# It prints the takeover times and request counts and exits non-zero on any
# failed read, any push that failed after its retries, or a slow takeover.
#
# Pushes are ?preflight=true (authenticated, recorded nowhere) as the `relay`
# reporter, with a token minted for this run; no upstream key is needed (the
# servers poll the keyless feeds and log the keyed ones as unavailable).
set -euo pipefail

cd "$(dirname "$0")/.."

PG_CONTAINER=${PG_CONTAINER:-sierra-data-pg}
PG_PORT=${PG_PORT:-55432}
DB=${REHEARSE_DB:-sierra_rehearsal}
PORT_A=${REHEARSE_PORT_A:-8197}
PORT_B=${REHEARSE_PORT_B:-8198}
TAKEOVER_MAX_MS=${REHEARSE_TAKEOVER_MAX_MS:-5000}
LOAD_SECONDS=${REHEARSE_LOAD_SECONDS:-6}
PUSH_RETRIES=${REHEARSE_PUSH_RETRIES:-10}
OUT=${REHEARSE_OUT:-$(mktemp -d "${TMPDIR:-/tmp}/rehearse-bluegreen.XXXXXX")}
URL="postgres://sierra:sierra@127.0.0.1:${PG_PORT}/${DB}?sslmode=disable"
STREAM=mesh.repeater

mkdir -p "$OUT"
TOKEN=$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')
TOKEN_SHA=$(printf %s "$TOKEN" | sha256sum | cut -d' ' -f1)
PIDS=()

log() { printf '%s %s\n' "$(date +%H:%M:%S.%3N)" "$*"; }
die() { log "FAIL: $*"; exit 1; }
now_ms() { date +%s%3N; }

cleanup() {
	for pid in "${PIDS[@]}"; do kill -9 "$pid" 2>/dev/null || true; done
	for pid in "${LOADERS[@]:-}"; do kill "$pid" 2>/dev/null || true; done
}
trap cleanup EXIT

psql_db() { docker exec "$PG_CONTAINER" psql -U sierra -d "$DB" -Atc "$1"; }

start_server() { # name port -> sets LAST_PID (a child of this shell, so `wait` works)
	local name=$1 port=$2
	PF__GRID__DATABASE__URL="$URL" PF__OPENAI__API_KEY=dummy \
		PF__SERVER__HOST=127.0.0.1 PF__SERVER__PORT="$port" \
		PF__GRID__INGEST__REPORTER_TOKEN_SHA256__RELAY="$TOKEN_SHA" \
		nohup ./bin/server >"$OUT/$name.log" 2>&1 &
	LAST_PID=$!
	PIDS+=("$LAST_PID")
}

readyz() { curl -s -m 2 "http://127.0.0.1:$1/readyz" || true; }

wait_for() { # description timeout-seconds command...
	local what=$1 limit=$2
	shift 2
	local deadline=$(($(date +%s) + limit))
	until "$@"; do
		(($(date +%s) < deadline)) || die "timed out waiting for $what"
		sleep 0.05
	done
}
is_ready() { [[ $(readyz "$1") == *'"ready":true'* ]]; }
is_writer() { [[ $(readyz "$1") == *'"writer":true'* ]]; }
is_reader() { [[ $(readyz "$1") == *'"ready":true,"writer":false'* ]]; }

push_once() { # port -> HTTP status
	curl -s -m 15 -o /dev/null -w '%{http_code}' -X POST \
		-H "Authorization: Bearer $TOKEN" \
		"http://127.0.0.1:$1/api/v1/ingest/$STREAM?preflight=true" || echo 000
}

# load port file: reads and pushes against one server until killed. Each line:
# <ms> read <status> | <ms> push <final status> <attempts>
load() {
	local port=$1 file=$2
	: >"$file"
	while true; do
		local code
		code=$(curl -s -m 5 -o /dev/null -w '%{http_code}' "http://127.0.0.1:$port/api/v1/events?pageSize=5" || echo 000)
		echo "$(now_ms) read $code" >>"$file"
		local attempt=1
		code=$(push_once "$port")
		while [[ $code == 503 && $attempt -le $PUSH_RETRIES ]]; do
			sleep 0.5 # Retry-After is 2s; a faster retry measures the gap more finely
			attempt=$((attempt + 1))
			code=$(push_once "$port")
		done
		echo "$(now_ms) push $code $attempt" >>"$file"
		sleep 0.1
	done
}

tally() { # file -> prints a summary, returns non-zero on failures
	local file=$1
	awk '
		$2 == "read" { reads++; if ($3 != 200) badreads++ }
		$2 == "push" { pushes++; if ($3 != 204) badpush++; if ($4 > 1) { retried++; extra += $4 - 1 } }
		END {
			printf "reads %d (failed %d), pushes %d (failed after retries %d, retried %d, extra attempts %d)\n",
				reads, badreads, pushes, badpush, retried, extra
			exit (badreads + badpush) > 0
		}' "$file"
}

LOADERS=()
start_load() { load "$1" "$2" & LOADERS+=($!); }
stop_load() { for pid in "${LOADERS[@]}"; do kill "$pid" 2>/dev/null || true; done; LOADERS=(); }

# --- setup -------------------------------------------------------------------
[[ -x bin/server ]] || die "bin/server is missing (make server)"
for port in "$PORT_A" "$PORT_B"; do
	if curl -s -m 1 -o /dev/null "http://127.0.0.1:$port/"; then die "port $port is already serving"; fi
done
log "database $DB (dropped and recreated), logs in $OUT"
docker exec "$PG_CONTAINER" psql -U sierra -d postgres -qc "DROP DATABASE IF EXISTS $DB WITH (FORCE)" >/dev/null
docker exec "$PG_CONTAINER" psql -U sierra -d postgres -qc \
	"CREATE DATABASE $DB TEMPLATE template0 LC_COLLATE 'C' LC_CTYPE 'C'" >/dev/null

# --- 1. A takes the lease ----------------------------------------------------
start_server a "$PORT_A"
PID_A=$LAST_PID
wait_for "A ready" 30 is_ready "$PORT_A"
wait_for "A to hold the lease" 30 is_writer "$PORT_A"
log "A ($PORT_A) serves and holds the lease: $(psql_db 'SELECT epoch, holder, address FROM writer_lease')"

# --- 2. B reads and forwards -------------------------------------------------
start_server b "$PORT_B"
PID_B=$LAST_PID
wait_for "B ready" 30 is_ready "$PORT_B"
is_reader "$PORT_B" || die "B should be a reader while A holds the lease: $(readyz "$PORT_B")"
code=$(curl -s -m 5 -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT_B/api/v1/sources")
[[ $code == 200 ]] || die "B /api/v1/sources answered $code"
wait_for "A's push ingest" 30 bash -c "[[ \$(curl -s -m 5 -o /dev/null -w '%{http_code}' -X POST -H 'Authorization: Bearer $TOKEN' 'http://127.0.0.1:$PORT_A/api/v1/ingest/$STREAM?preflight=true') == 204 ]]"
code=$(push_once "$PORT_B")
[[ $code == 204 ]] || die "a push to B answered $code (expected 204, forwarded to A)"
grep -q 'Push ingest: forwarded to the writer' "$OUT/b.log" || die "B did not log the forward"
body_a=$(curl -s "http://127.0.0.1:$PORT_A/api/v1/sources" | sha256sum)
body_b=$(curl -s "http://127.0.0.1:$PORT_B/api/v1/sources" | sha256sum)
log "B ($PORT_B) is a ready reader, serves reads, forwarded a push to A (204); /sources bodies equal: $([[ $body_a == "$body_b" ]] && echo yes || echo NO)"

# --- 3. SIGTERM to A under load on B ------------------------------------------
start_load "$PORT_B" "$OUT/load-sigterm.txt"
sleep 2
t0=$(now_ms)
kill -TERM "$PID_A"
wait_for "B to take the lease after SIGTERM" 30 is_writer "$PORT_B"
t1=$(now_ms)
TAKEOVER_TERM=$((t1 - t0))
log "SIGTERM to A: B holds the lease after ${TAKEOVER_TERM} ms"
sleep "$LOAD_SECONDS"
stop_load
wait "$PID_A" 2>/dev/null && exit_a=0 || exit_a=$?
SUMMARY_TERM=$(tally "$OUT/load-sigterm.txt") && ok_term=1 || ok_term=0
log "  load on B across the switch: $SUMMARY_TERM; A exited $exit_a"

# --- 4. A back as a reader; SIGKILL to B under load on A ----------------------
start_server a2 "$PORT_A"
PID_A=$LAST_PID
wait_for "A ready again" 30 is_ready "$PORT_A"
is_reader "$PORT_A" || die "A should come back as a reader: $(readyz "$PORT_A")"
wait_for "B's push ingest" 30 bash -c "[[ \$(curl -s -m 5 -o /dev/null -w '%{http_code}' -X POST -H 'Authorization: Bearer $TOKEN' 'http://127.0.0.1:$PORT_B/api/v1/ingest/$STREAM?preflight=true') == 204 ]]"
start_load "$PORT_A" "$OUT/load-sigkill.txt"
sleep 2
t0=$(now_ms)
kill -KILL "$PID_B"
wait_for "A to take the lease after SIGKILL" 30 is_writer "$PORT_A"
t1=$(now_ms)
TAKEOVER_KILL=$((t1 - t0))
log "SIGKILL to B: A holds the lease after ${TAKEOVER_KILL} ms"
sleep "$LOAD_SECONDS"
stop_load
SUMMARY_KILL=$(tally "$OUT/load-sigkill.txt") && ok_kill=1 || ok_kill=0
log "  load on A across the crash: $SUMMARY_KILL"

# --- 5. the lease record -------------------------------------------------------
lease=$(psql_db 'SELECT epoch, holder <> $$$$, address FROM writer_lease')
locks=$(psql_db "SELECT COUNT(*) FROM pg_locks WHERE locktype = 'advisory' AND granted AND database = (SELECT oid FROM pg_database WHERE datname = '$DB')")
log "lease now: $lease; advisory locks held: $locks"
epoch=${lease%%|*}
kill -TERM "$PID_A"
wait "$PID_A" 2>/dev/null || true
after=$(psql_db 'SELECT holder, address FROM writer_lease')

fail=0
[[ $epoch == 3 ]] || { log "FAIL: expected epoch 3 (A, B, A), got $epoch"; fail=1; }
[[ $locks == 1 ]] || { log "FAIL: expected exactly one advisory lock holder, got $locks"; fail=1; }
[[ $after == "|" ]] || { log "FAIL: a clean SIGTERM should clear holder and address, got '$after'"; fail=1; }
[[ $exit_a == 0 ]] || { log "FAIL: A exited $exit_a after SIGTERM"; fail=1; }
((TAKEOVER_TERM <= TAKEOVER_MAX_MS)) || { log "FAIL: SIGTERM takeover ${TAKEOVER_TERM} ms > ${TAKEOVER_MAX_MS} ms"; fail=1; }
((TAKEOVER_KILL <= TAKEOVER_MAX_MS)) || { log "FAIL: SIGKILL takeover ${TAKEOVER_KILL} ms > ${TAKEOVER_MAX_MS} ms"; fail=1; }
((ok_term && ok_kill)) || { log "FAIL: requests failed (beyond the push retry) during a switch"; fail=1; }

echo
echo "rehearse-bluegreen: SIGTERM takeover ${TAKEOVER_TERM} ms; SIGKILL takeover ${TAKEOVER_KILL} ms"
echo "  SIGTERM: $SUMMARY_TERM"
echo "  SIGKILL: $SUMMARY_KILL"
echo "  logs: $OUT"
((fail == 0)) && echo "PASS" || { echo "FAIL"; exit 1; }
