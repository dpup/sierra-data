#!/usr/bin/env python3
"""Diff the Grid API of two servers (migration plan section 8, gate 2).

Run the legacy SQLite-backed server and the new PostgreSQL-backed server on
the same snapshot (the new one loaded through `gridctl import`), then:

    python3 scripts/apidiff.py --legacy http://localhost:8199 --new http://localhost:8200
    make apidiff LEGACY=http://localhost:8199 NEW=http://localhost:8200

Every endpoint in ENDPOINTS is fetched from both, paged lists are walked to
the end (or --max-pages) and compared as one list, volatile fields are
dropped (VOLATILE_KEYS, VOLATILE_BY_PATH), lists the API does not promise an
order for are sorted (UNORDERED), and each difference is printed as a unified
diff. Event ids found on the legacy side add per-event endpoints (detail,
history, related, timeline) for a sample of --events ids.

Exit status: 0 no differences, 1 differences, 2 a server was unreachable.
Any difference is a finding to explain, not to wave through: both servers
should run with pollers that cannot change the data during the run (no
upstream credentials, or the new writer disabled), or the run should be
repeated until it is quiet.

Python 3 standard library only, like scripts/deploy.py.
"""

import argparse
import difflib
import json
import sys
import urllib.error
import urllib.parse
import urllib.request

API = "/api/v1"
SAMPLE_PLACE = "calaveras-county"

# The endpoint list, in one place. Each entry: (name, path with query, the
# key holding the page's items when the endpoint pages, else None).
ENDPOINTS = [
    ("events default", "/events?page_size=200", "events"),
    ("events all statuses", "/events?status=ACTIVE,SCHEDULED,RESOLVED,EXPIRED&page_size=200", "events"),
    ("events lite", "/events?status=ACTIVE,SCHEDULED,RESOLVED,EXPIRED&geometry_format=NONE&page_size=200", "events"),
    ("events road incidents", "/events?layer=ROAD_INCIDENT&status=ACTIVE,RESOLVED&page_size=200", "events"),
    ("events weather alerts", "/events?layer=WEATHER_ALERT&status=ACTIVE,EXPIRED,RESOLVED&page_size=200", "events"),
    ("events mesh", "/events?layer=MESH&status=ACTIVE,EXPIRED&page_size=200", "events"),
    ("events place", "/events?place=" + SAMPLE_PLACE + "&status=ACTIVE,SCHEDULED,RESOLVED,EXPIRED&page_size=200", "events"),
    ("events severity", "/events?severity_min=MODERATE&status=ACTIVE,RESOLVED&page_size=200", "events"),
    ("events since", "/events?since=2026-10-01T00:00:00Z&status=ACTIVE,RESOLVED,EXPIRED&page_size=200", "events"),
    ("history", "/history?page_size=200", "revisions"),
    ("history wildfire", "/history?layer=WILDFIRE&page_size=200", "revisions"),
    ("history place", "/history?place=" + SAMPLE_PLACE + "&page_size=200", "revisions"),
    ("places", "/places", None),
    ("places counties", "/places?kind=COUNTY", None),
    ("place", "/places/" + SAMPLE_PLACE, None),
    ("place summary", "/places/" + SAMPLE_PLACE + "/summary", None),
    ("places resolve", "/places:resolve?lat=38.2555&lng=-120.3513", None),
    ("conditions", "/conditions", None),
    ("sources", "/sources", None),
    ("stats", "/stats", None),
    ("mesh links", "/mesh/links", None),
] + [
    ("map " + layer, "/places/" + SAMPLE_PLACE + "/map/" + layer + ".geojson", None)
    for layer in ("wildfire", "evacuation", "weather_alert", "earthquake", "road_incident", "power",
                  "mesh_node", "mesh_link")
]

# Per-event endpoints, added for each sampled event id.
EVENT_ENDPOINTS = [
    ("event", "/events/{id}", None),
    ("event history", "/events/{id}/history?page_size=200", "revisions"),
    ("event related", "/events/{id}/related", None),
    ("event timeline", "/events/{id}/timeline", None),
]

# Keys dropped wherever they appear: they change with the clock, not with
# the data (fetch and generation times).
VOLATILE_KEYS = {"generatedAt", "fetchedAt", "lastSourceUpdate"}

# Keys dropped only under some endpoints: source health moves with every
# poll attempt, and conditions are a live upstream read rather than stored
# data.
VOLATILE_BY_PATH = {
    "/sources": {"lastAttemptAt", "lastSuccessAt", "lastError", "status"},
    "/conditions": {"observedAt", "updatedAt", "lastUpdated", "timestamp"},
    "/summary": {"lastAttemptAt", "lastSuccessAt", "lastError"},
}

# Lists the API does not promise an order for, sorted before comparing:
# endpoint substring -> (key of the list, key to sort its items by).
UNORDERED = {
    "/sources": ("sources", "id"),
    "/mesh/links": ("features", "id"),
    "mesh_link.geojson": ("features", "id"),
}


def fetch(url, timeout=60):
    """GET url; returns (status, body parsed as JSON, or text)."""
    req = urllib.request.Request(url, headers={"Accept": "application/json", "User-Agent": "sierra-apidiff"})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            status, raw = resp.status, resp.read()
    except urllib.error.HTTPError as err:
        status, raw = err.code, err.read()
    text = raw.decode("utf-8", errors="replace")
    try:
        return status, json.loads(text)
    except ValueError:
        return status, text


def with_query(path, **params):
    parts = urllib.parse.urlsplit(path)
    query = urllib.parse.parse_qsl(parts.query, keep_blank_values=True)
    query = [(k, v) for k, v in query if k not in params] + [(k, v) for k, v in params.items()]
    return urllib.parse.urlunsplit(("", "", parts.path, urllib.parse.urlencode(query, safe=",:"), ""))


def get(fetcher, base, path, items_key, max_pages):
    """Fetches one endpoint. A paged one is walked and returned as
    {"status", "items", "pages", "truncated"}; nextPageToken values are not
    compared (they encode cursors, which the items already cover)."""
    status, body = fetcher(base.rstrip("/") + API + path)
    if items_key is None or status != 200 or not isinstance(body, dict):
        return {"status": status, "body": body}
    items, pages = list(body.get(items_key) or []), 1
    token = body.get("nextPageToken") or ""
    while token and pages < max_pages:
        status, body = fetcher(base.rstrip("/") + API + with_query(path, page_token=token))
        if status != 200 or not isinstance(body, dict):
            return {"status": status, "body": body, "items": items, "pages": pages}
        items += body.get(items_key) or []
        pages += 1
        token = body.get("nextPageToken") or ""
    return {"status": 200, "items": items, "pages": pages, "truncated": bool(token)}


def normalize(value, path):
    """Drops volatile keys and sorts unordered lists, recursively."""
    drop = set(VOLATILE_KEYS)
    for fragment, keys in VOLATILE_BY_PATH.items():
        if fragment in path:
            drop |= keys
    unordered = [spec for fragment, spec in UNORDERED.items() if fragment in path]

    def walk(v):
        if isinstance(v, dict):
            out = {k: walk(x) for k, x in v.items() if k not in drop}
            for list_key, sort_key in unordered:
                if isinstance(out.get(list_key), list):
                    out[list_key] = sorted(out[list_key], key=lambda item: json.dumps(
                        item.get(sort_key) if isinstance(item, dict) else item, sort_keys=True)
                        + json.dumps(item, sort_keys=True))
            return out
        if isinstance(v, list):
            return [walk(x) for x in v]
        return v

    return walk(value)


def render(value):
    return json.dumps(value, indent=1, sort_keys=True, ensure_ascii=False).splitlines()


def compare(name, path, legacy, new, max_lines):
    """Returns the diff lines for one endpoint ([] when equal)."""
    a, b = render(normalize(legacy, path)), render(normalize(new, path))
    if a == b:
        return []
    diff = list(difflib.unified_diff(a, b, "legacy " + path, "new " + path, n=2, lineterm=""))
    if len(diff) > max_lines:
        diff = diff[:max_lines] + ["... (%d more diff lines)" % (len(diff) - max_lines)]
    return ["== DIFF %s: %s" % (name, path)] + diff


def sample_event_ids(result, limit):
    """Spreads `limit` picks over the legacy side's event list."""
    ids = [e.get("id") for e in result.get("items") or [] if isinstance(e, dict) and e.get("id")]
    if limit <= 0 or len(ids) <= limit:
        return ids if limit != 0 else []
    step = len(ids) / limit
    return [ids[int(i * step)] for i in range(limit)]


def run(legacy_base, new_base, events=25, max_pages=50, max_lines=60, fetcher=fetch, out=sys.stdout):
    endpoints = list(ENDPOINTS)
    differing, compared, unreachable = 0, 0, []

    def one(name, path, items_key):
        nonlocal differing, compared
        try:
            a = get(fetcher, legacy_base, path, items_key, max_pages)
            b = get(fetcher, new_base, path, items_key, max_pages)
        except (urllib.error.URLError, OSError) as err:
            unreachable.append("%s: %s" % (path, err))
            return None
        compared += 1
        lines = compare(name, path, a, b, max_lines)
        if lines:
            differing += 1
            print("\n".join(lines), file=out)
        if a.get("status") != 200 or b.get("status") != 200:
            print("== NOTE %s: HTTP %s (legacy) / %s (new)" % (path, a.get("status"), b.get("status")), file=out)
        if a.get("truncated") or b.get("truncated"):
            print("== NOTE %s: stopped after %d pages" % (path, max_pages), file=out)
        return a

    walked = None
    for name, path, items_key in endpoints:
        result = one(name, path, items_key)
        if name == "events all statuses":
            walked = result
    for event_id in sample_event_ids(walked or {}, events):
        quoted = urllib.parse.quote(event_id, safe="")
        for name, path, items_key in EVENT_ENDPOINTS:
            one(name, path.replace("{id}", quoted), items_key)

    for line in unreachable:
        print("== UNREACHABLE " + line, file=out)
    print("apidiff: %d endpoints compared, %d differ, %d unreachable" % (compared, differing, len(unreachable)),
          file=out)
    if unreachable:
        return 2
    return 1 if differing else 0


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--legacy", required=True, help="base URL of the legacy (SQLite) server")
    parser.add_argument("--new", required=True, help="base URL of the new (PostgreSQL) server")
    parser.add_argument("--events", type=int, default=25,
                        help="event ids to sample for per-event endpoints (-1 = every event, 0 = none)")
    parser.add_argument("--max-pages", type=int, default=50, help="page limit per paged endpoint")
    parser.add_argument("--max-lines", type=int, default=60, help="diff lines printed per endpoint")
    args = parser.parse_args(argv)
    return run(args.legacy, args.new, args.events, args.max_pages, args.max_lines)


if __name__ == "__main__":
    sys.exit(main())
