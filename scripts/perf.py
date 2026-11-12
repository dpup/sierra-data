#!/usr/bin/env python3
"""Latency of the hot read endpoints, sequential over one keep-alive
connection: p50/p95/max over N requests each (default 200), after a short
warm-up. Used for the PostgreSQL performance pass (gate 4); run it against a
server on an imported production-sized database, before and after a change:

    python3 scripts/perf.py --base http://127.0.0.1:8197 [--place calaveras-county] \
        [--n 200] [--json out.json] [--concurrent-stats 16]

Event ids are discovered from the API itself (the busiest mesh event by
revision count for /events/{id} and its history). Python 3 standard library
only. Latencies are wall-clock milliseconds as the client sees them.
"""

import argparse
import concurrent.futures
import http.client
import json
import statistics
import sys
import threading
import time
import urllib.parse

API = "/api/v1"


def conn(base):
    u = urllib.parse.urlparse(base)
    return http.client.HTTPConnection(u.hostname, u.port, timeout=60)


def get(c, path):
    t = time.perf_counter()
    c.request("GET", path)
    r = c.getresponse()
    body = r.read()
    return (time.perf_counter() - t) * 1000.0, r.status, len(body), body


def pct(xs, p):
    xs = sorted(xs)
    return xs[min(len(xs) - 1, max(0, int(round(p / 100.0 * len(xs) + 0.5)) - 1))]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", required=True)
    ap.add_argument("--place", default="calaveras-county", help="a busy place slug")
    ap.add_argument("--corridor", default="hwy4-murphys-arnold", help="a corridor place slug")
    ap.add_argument("--n", type=int, default=200)
    ap.add_argument("--json", help="also write the results here")
    ap.add_argument("--concurrent-stats", type=int, default=0,
                    help="after the table, fire this many simultaneous /stats requests")
    ap.add_argument("--burst-only", action="store_true", help="skip the table, run only --concurrent-stats")
    a = ap.parse_args()

    c = conn(a.base)
    _, st, _, body = get(c, API + "/events?layer=MESH&pageSize=200")
    if st != 200:
        sys.exit("cannot list events: %d" % st)
    evs = json.loads(body).get("events", [])
    if not evs:
        sys.exit("no events to sample")
    ev = max(evs, key=lambda e: int(e.get("revision", 1)))
    eid = urllib.parse.quote(ev["id"], safe="")

    p, q = a.place, urllib.parse.quote
    cases = [
        ("events (default)", "/events"),
        ("events layer=road_incident&place=%s" % a.corridor,
         "/events?layer=road_incident&place=%s" % a.corridor),
        ("events layer=road_incident&place=%s" % p, "/events?layer=road_incident&place=%s" % p),
        ("events place=%s" % p, "/events?place=%s" % p),
        ("events/{id}", "/events/%s" % eid),
        ("events/{id}/history (rev %s)" % ev.get("revision", "?"), "/events/%s/history" % eid),
        ("history?place=%s" % p, "/history?place=%s" % p),
        ("places/%s/summary" % p, "/places/%s/summary" % p),
        ("places/%s/map/wildfire.geojson" % p, "/places/%s/map/wildfire.geojson" % p),
        ("places/%s/map/weather_alert.geojson" % p, "/places/%s/map/weather_alert.geojson" % p),
        ("stats", "/stats"),
        ("stats?place=%s" % p, "/stats?place=%s" % p),
        ("mesh/links", "/mesh/links"),
    ]
    out = []
    if a.burst_only:
        cases = []
    print("%-52s %8s %8s %8s %8s %8s %9s" % ("endpoint", "1st ms", "p50 ms", "p95 ms", "p99 ms", "max ms", "bytes"))
    for name, path in cases:
        first = get(c, API + path)[0]  # cold: a /stats cache miss, cold pool, plan cache
        for _ in range(4):
            get(c, API + path)
        lat, size, bad = [], 0, 0
        for _ in range(a.n):
            ms, status, n, _ = get(c, API + path)
            lat.append(ms)
            size = n
            bad += status != 200
        row = dict(endpoint=name, first=first, p50=pct(lat, 50), p95=pct(lat, 95), p99=pct(lat, 99),
                   max=max(lat), mean=statistics.fmean(lat), bytes=size, non200=bad)
        out.append(row)
        print("%-52s %8.1f %8.1f %8.1f %8.1f %8.1f %9d%s" % (name, first, row["p50"], row["p95"], row["p99"],
                                                       row["max"], size, "  NON-200 x%d" % bad if bad else ""))
    if a.concurrent_stats:
        k = a.concurrent_stats

        barrier = threading.Barrier(k)

        def one(_):
            cc = conn(a.base)
            cc.connect()
            barrier.wait()
            try:
                return get(cc, API + "/stats?place=%s" % p)[0]
            finally:
                cc.close()

        t = time.perf_counter()
        with concurrent.futures.ThreadPoolExecutor(k) as ex:
            lats = list(ex.map(one, range(k)))
        print("concurrent /stats x%d: wall %.1f ms, per-request p50 %.1f max %.1f ms" % (
            k, (time.perf_counter() - t) * 1000, pct(lats, 50), max(lats)))
        out.append(dict(endpoint="concurrent stats x%d" % k, p50=pct(lats, 50), max=max(lats)))
    if a.json:
        with open(a.json, "w") as f:
            json.dump(out, f, indent=1)


if __name__ == "__main__":
    main()
