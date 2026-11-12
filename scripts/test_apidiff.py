import io
import json
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import urllib.parse

import apidiff


class FakeServer:
    """A fetcher over an in-memory route table: path (with query) -> body."""

    def __init__(self, routes, default=None):
        self.routes, self.default, self.calls = routes, default, []

    def __call__(self, url):
        parts = urllib.parse.urlsplit(url)
        key = parts.path[len(apidiff.API):] + ("?" + parts.query if parts.query else "")
        self.calls.append(key)
        for route, body in self.routes.items():
            if key == route or (route.endswith("*") and key.startswith(route[:-1])):
                return 200, body
        if self.default is not None:
            return 200, self.default
        return 404, {"code": 5, "message": "not found"}


def paged(items, size, key="events"):
    """Routes for a paged /events listing split into pages of `size`."""
    routes = {}
    path = "/events?status=ACTIVE,SCHEDULED,RESOLVED,EXPIRED&page_size=200"
    for i in range(0, len(items), size):
        token = "t%d" % (i + size) if i + size < len(items) else ""
        page = {key: items[i:i + size], "nextPageToken": token}
        routes[path if i == 0 else apidiff.with_query(path, page_token="t%d" % i)] = page
    return routes


class NormalizeTests(unittest.TestCase):
    def test_volatile_keys_are_dropped_everywhere(self):
        doc = {"metadata": {"generatedAt": "now", "count": 1}, "events": [{"provenance": {"fetchedAt": "x", "sourceId": "chp"}}]}
        self.assertEqual(apidiff.normalize(doc, "/events"),
                         {"metadata": {"count": 1}, "events": [{"provenance": {"sourceId": "chp"}}]})

    def test_source_health_is_ignored_only_on_sources(self):
        doc = {"sources": [{"id": "chp", "status": "OK", "lastAttemptAt": "t"}]}
        self.assertEqual(apidiff.normalize(doc, "/sources"), {"sources": [{"id": "chp"}]})
        event = {"status": "ACTIVE"}
        self.assertEqual(apidiff.normalize(event, "/events/x"), event, "an event's status is data")

    def test_stats_computation_time_is_ignored(self):
        doc = {"events": 3, "computedAt": "2026-11-12T00:00:00Z"}
        self.assertEqual(apidiff.normalize(doc, "/stats"), {"events": 3})

    def test_unordered_lists_are_sorted(self):
        a = {"sources": [{"id": "usgs"}, {"id": "chp"}]}
        b = {"sources": [{"id": "chp"}, {"id": "usgs"}]}
        self.assertEqual(apidiff.normalize(a, "/sources"), apidiff.normalize(b, "/sources"))
        events_a = {"events": [{"id": "b"}, {"id": "a"}]}
        self.assertNotEqual(apidiff.normalize(events_a, "/events"),
                            apidiff.normalize({"events": [{"id": "a"}, {"id": "b"}]}, "/events"),
                            "event order is promised, so it is compared")


class WalkTests(unittest.TestCase):
    def test_pages_are_concatenated_and_tokens_not_compared(self):
        items = [{"id": "e%d" % i} for i in range(5)]
        server = FakeServer(paged(items, 2))
        path = "/events?status=ACTIVE,SCHEDULED,RESOLVED,EXPIRED&page_size=200"
        got = apidiff.get(server, "http://x", path, "events", 10)
        self.assertEqual(got["items"], items)
        self.assertEqual(got["pages"], 3)
        self.assertFalse(got["truncated"])

    def test_max_pages_truncates(self):
        server = FakeServer(paged([{"id": "e%d" % i} for i in range(5)], 1))
        path = "/events?status=ACTIVE,SCHEDULED,RESOLVED,EXPIRED&page_size=200"
        got = apidiff.get(server, "http://x", path, "events", 2)
        self.assertEqual(len(got["items"]), 2)
        self.assertTrue(got["truncated"])

    def test_sample_event_ids_spreads_picks(self):
        result = {"items": [{"id": str(i)} for i in range(10)]}
        self.assertEqual(apidiff.sample_event_ids(result, 5), ["0", "2", "4", "6", "8"])
        self.assertEqual(len(apidiff.sample_event_ids(result, -1)), 10)
        self.assertEqual(apidiff.sample_event_ids(result, 0), [])


class RunTests(unittest.TestCase):
    def run_diff(self, legacy, new, **kw):
        out = io.StringIO()
        code = apidiff.run("http://legacy", "http://new", fetcher=lambda url: (legacy if "legacy" in url else new)(url),
                           out=out, **kw)
        return code, out.getvalue()

    def test_identical_servers_exit_zero(self):
        items = [{"id": "chp:1", "headline": "a"}, {"id": "chp:2", "headline": "b"}]
        routes = paged(items, 1)
        server = FakeServer(routes, default={"ok": True, "generatedAt": "volatile"})
        other = FakeServer(dict(routes), default={"ok": True, "generatedAt": "different"})
        code, out = self.run_diff(server, other, events=-1)
        self.assertEqual(code, 0, out)
        self.assertIn("0 differ", out)
        self.assertIn("/events/chp%3A1/history?page_size=200", server.calls, "per-event endpoints follow the walk")

    def test_a_difference_is_printed_and_exits_one(self):
        legacy = FakeServer(paged([{"id": "chp:1", "severity": "MINOR"}], 1), default={})
        new = FakeServer(paged([{"id": "chp:1", "severity": "SEVERE"}], 1), default={})
        code, out = self.run_diff(legacy, new, events=0)
        self.assertEqual(code, 1)
        self.assertIn("== DIFF events all statuses", out)
        self.assertIn('-   "severity": "MINOR"', out)
        self.assertIn('+   "severity": "SEVERE"', out)

    def test_status_codes_are_compared(self):
        legacy = FakeServer({}, default={"x": 1})
        new = FakeServer({"/stats": {"x": 1}})
        code, out = self.run_diff(legacy, new, events=0)
        self.assertEqual(code, 1)
        self.assertIn("404", out)

    def test_unreachable_exits_two(self):
        def down(url):
            raise OSError("connection refused")
        code, out = self.run_diff(down, down, events=0)
        self.assertEqual(code, 2)
        self.assertIn("UNREACHABLE", out)


class HTTPTests(unittest.TestCase):
    """The real fetcher against a local server (no network)."""

    def test_fetch_reads_json_and_error_bodies(self):
        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                code = 404 if "missing" in self.path else 200
                body = json.dumps({"path": self.path}).encode()
                self.send_response(code)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *args):
                pass

        server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        try:
            base = "http://127.0.0.1:%d" % server.server_address[1]
            self.assertEqual(apidiff.fetch(base + "/api/v1/stats"), (200, {"path": "/api/v1/stats"}))
            self.assertEqual(apidiff.fetch(base + "/missing")[0], 404)
        finally:
            server.shutdown()
            server.server_close()


if __name__ == "__main__":
    unittest.main()
