#!/usr/bin/env python3
"""Local two-site regression for gospider --request-budget.

Spins up two local HTTP sites with known routes (HTML links, a JS file for
linkfinder, robots.txt, sitemap.xml, a redirect chain, a dead link and a
filtered-by-extension link), then runs gospider with different
--request-budget values and cross-checks:

  * the per-site closing JSON summary (used / truncated)
  * the actual number of HTTP requests observed by the servers (ground truth)
  * budget 0 keeps the original behavior (no summary, full crawl)
  * budget 1 and concurrent crawls never exceed the budget
  * redirect hops and failed attempts are charged, deduped/filtered URLs are not
  * a negative budget fails before startup (zero requests made)
  * quiet mode keeps stdout URL-only (summary goes to stderr)
  * json mode keeps every stdout line parseable

Usage:
    python3 regression.py [path-to-gospider-binary]

The binary defaults to $GOSPIDER_BIN or ./gospider.
"""

import json
import os
import re
import socket
import subprocess
import sys
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

GOSPIDER = sys.argv[1] if len(sys.argv) > 1 else os.environ.get("GOSPIDER_BIN", "./gospider")

SITEMAP_PATHS = [
    "/sitemap.xml", "/sitemap_news.xml", "/sitemap_index.xml", "/sitemap-index.xml",
    "/sitemapindex.xml", "/sitemap-news.xml", "/post-sitemap.xml", "/page-sitemap.xml",
    "/portfolio-sitemap.xml", "/home_slider-sitemap.xml", "/category-sitemap.xml",
    "/author-sitemap.xml",
]

HITS_LOCK = threading.Lock()
HITS = {"A": [], "B": []}

FAILURES = []  # assertion failures collected across cases


def record(site, path):
    with HITS_LOCK:
        HITS[site].append(path)


def hits_since(site, snapshot):
    with HITS_LOCK:
        return list(HITS[site][snapshot:])


def snapshot():
    with HITS_LOCK:
        return {k: len(v) for k, v in HITS.items()}


def make_handler(site):
    class Handler(BaseHTTPRequestHandler):
        def _send(self, code, body=b"", ctype="text/html", extra=None):
            self.send_response(code)
            self.send_header("Content-Type", ctype)
            if extra:
                for k, v in extra.items():
                    self.send_header(k, v)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            if body:
                self.wfile.write(body)

        def do_GET(self):
            record(site, self.path)
            srv = self.server
            if site == "A":
                self.route_a(srv)
            else:
                self.route_b(srv)

        def route_a(self, srv):
            dead = srv.dead_port
            if self.path == "/":
                body = (
                    '<html>'
                    '<a href="/p1">p1</a>'
                    '<a href="/p2">p2a</a>'
                    '<a href="/p2">p2b</a>'          # duplicate: must be requested once
                    '<a href="/redir">redir</a>'
                    f'<a href="http://127.0.0.1:{dead}/gone">dead</a>'  # failed attempt
                    '<a href="/img.png">img</a>'     # filtered extension: never sent
                    '<script src="/app.js"></script>'
                    '</html>'
                ).encode()
                self._send(200, body)
            elif self.path == "/p1":
                self._send(200, b'<html><a href="/p3">p3</a></html>')
            elif self.path in ("/p2", "/p3", "/rob1", "/sm1", "/jsfound1", "/r3"):
                self._send(200, b"ok")
            elif self.path == "/redir":
                self._send(302, extra={"Location": "/r2"})
            elif self.path == "/r2":
                self._send(302, extra={"Location": "/r3"})
            elif self.path == "/rstart":
                self._send(200, b'<html><a href="/redir">redir</a></html>')
            elif self.path == "/app.js":
                self._send(200, b'var u = "/jsfound1";', "application/javascript")
            elif self.path == "/robots.txt":
                self._send(200, b"User-agent: *\nDisallow: /rob1\n", "text/plain")
            elif self.path == "/sitemap.xml":
                loc = f"http://127.0.0.1:{srv.server_address[1]}/sm1"
                body = (
                    '<?xml version="1.0" encoding="UTF-8"?>'
                    '<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">'
                    f"<url><loc>{loc}</loc></url></urlset>"
                ).encode()
                self._send(200, body, "application/xml")
            else:
                self._send(404, b"nope")

        def route_b(self, srv):
            if self.path == "/":
                self._send(200, b'<html><a href="/b1">b1</a></html>')
            elif self.path == "/b1":
                self._send(200, b"ok")
            else:
                self._send(404, b"nope")

        def log_message(self, *args):
            pass

    return Handler


def start_server(site):
    srv = ThreadingHTTPServer(("127.0.0.1", 0), make_handler(site))
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv


def run_gospider(args, sites):
    """Run gospider against the given site URLs, return (rc, stdout, stderr)."""
    env = dict(os.environ)
    # Make sure local test traffic never goes through a proxy.
    env["NO_PROXY"] = "127.0.0.1,localhost"
    env["no_proxy"] = "127.0.0.1,localhost"
    with tempfile.NamedTemporaryFile("w", suffix=".txt", delete=False) as f:
        f.write("\n".join(sites) + "\n")
        sites_file = f.name
    try:
        proc = subprocess.run(
            [GOSPIDER, "-S", sites_file, "-t", str(len(sites)), "-m", "5"] + args,
            capture_output=True, text=True, timeout=120, env=env,
        )
        return proc.returncode, proc.stdout, proc.stderr
    finally:
        os.unlink(sites_file)


def summaries(text):
    """Extract and JSON-parse the request-budget closing lines."""
    out = []
    for line in text.splitlines():
        if '"type":"request-budget"' in line:
            out.append(json.loads(line))
    return out


def check(name, cond, detail=""):
    status = "PASS" if cond else "FAIL"
    print(f"[{status}] {name}" + (f" -- {detail}" if detail and not cond else ""))
    if not cond:
        FAILURES.append(f"{name}: {detail}")


def main():
    if not os.path.exists(GOSPIDER):
        print(f"gospider binary not found: {GOSPIDER}")
        sys.exit(2)

    # A port that is guaranteed closed: failed attempts land here.
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    dead_port = s.getsockname()[1]
    s.close()

    srv_a = start_server("A")
    srv_b = start_server("B")
    srv_a.dead_port = dead_port
    srv_b.dead_port = dead_port
    site_a = f"http://127.0.0.1:{srv_a.server_address[1]}"
    site_b = f"http://127.0.0.1:{srv_b.server_address[1]}"
    print(f"site A: {site_a}   site B: {site_b}   dead port: {dead_port}")

    # ------------------------------------------------------------------
    # Case 1: negative budget is rejected before startup, zero requests made
    # ------------------------------------------------------------------
    snap = snapshot()
    rc, out, err = run_gospider(["--request-budget=-1"], [site_a])
    check("negative budget exits non-zero", rc != 0, f"rc={rc}")
    check("negative budget reports error", "request-budget" in err, f"stderr={err!r}")
    check("negative budget sends no requests",
          hits_since("A", snap["A"]) == [] and hits_since("B", snap["B"]) == [])

    # ------------------------------------------------------------------
    # Case 2: budget 0 = original behavior, no summary, full two-site crawl
    # ------------------------------------------------------------------
    snap = snapshot()
    rc, out, err = run_gospider(["-d", "3", "--sitemap", "--request-budget", "0"],
                                [site_a, site_b])
    hits_a, hits_b = hits_since("A", snap["A"]), hits_since("B", snap["B"])
    expected_a = sorted(
        ["/", "/robots.txt", "/rob1", "/sm1", "/p1", "/p2", "/p3",
         "/redir", "/r2", "/r3", "/app.js", "/jsfound1"] + SITEMAP_PATHS)
    expected_b = sorted(["/", "/robots.txt", "/b1"] + SITEMAP_PATHS)
    check("budget 0: exit ok", rc == 0, f"rc={rc}")
    check("budget 0: no budget summary anywhere",
          "request-budget" not in out and "request-budget" not in err)
    check("budget 0: site A requests exactly as expected",
          sorted(hits_a) == expected_a,
          f"extra={sorted(set(hits_a) - set(expected_a))} "
          f"missing={sorted(set(expected_a) - set(hits_a))}")
    check("budget 0: site B requests exactly as expected",
          sorted(hits_b) == expected_b,
          f"extra={sorted(set(hits_b) - set(expected_b))} "
          f"missing={sorted(set(expected_b) - set(hits_b))}")
    check("budget 0: filtered extension never requested", "/img.png" not in hits_a)
    check("budget 0: duplicate link requested once", hits_a.count("/p2") == 1)
    check("budget 0: normal output contract kept",
          "[url] - [code-200]" in out and "[href] -" in out)

    # ------------------------------------------------------------------
    # Case 3: budget 1 per site, two sites in parallel
    # ------------------------------------------------------------------
    snap = snapshot()
    rc, out, err = run_gospider(["-d", "3", "--sitemap", "-c", "4",
                                 "--request-budget", "1"], [site_a, site_b])
    sums = summaries(out)
    hits_a, hits_b = hits_since("A", snap["A"]), hits_since("B", snap["B"])
    check("budget 1: one summary per site", len(sums) == 2, f"got {len(sums)}")
    check("budget 1: each site used exactly 1",
          all(s["used"] == 1 and s["budget"] == 1 for s in sums), f"{sums}")
    check("budget 1: each site truncated", all(s["truncated"] for s in sums), f"{sums}")
    check("budget 1: inputs are the two sites",
          sorted(s["input"] for s in sums) == sorted([site_a, site_b]), f"{sums}")
    check("budget 1: exactly one request reached each site (budgets independent)",
          len(hits_a) == 1 and len(hits_b) == 1,
          f"A={hits_a} B={hits_b}")

    # ------------------------------------------------------------------
    # Case 4: budget 7 per site under high concurrency, never exceeded
    # ------------------------------------------------------------------
    snap = snapshot()
    rc, out, err = run_gospider(["-d", "3", "--sitemap", "-c", "8",
                                 "--request-budget", "7"], [site_a, site_b])
    sums = summaries(out)
    hits_a, hits_b = hits_since("A", snap["A"]), hits_since("B", snap["B"])
    check("budget 7: each site used exactly 7",
          len(sums) == 2 and all(s["used"] == 7 for s in sums), f"{sums}")
    check("budget 7: each site truncated", all(s["truncated"] for s in sums), f"{sums}")
    # At most one attempt can fail invisibly (the dead-port link); every other
    # charged attempt must be observable at the server, and never above budget.
    check("budget 7: site A server saw <= 7 and >= 6 requests",
          6 <= len(hits_a) <= 7, f"A hits={len(hits_a)}")
    check("budget 7: site B server saw exactly 7 requests (no dead link there)",
          len(hits_b) == 7, f"B hits={len(hits_b)}")

    # ------------------------------------------------------------------
    # Case 5: redirect chain - every hop shares the budget
    # ------------------------------------------------------------------
    # /rstart links only to /redir -> /r2 -> /r3 (3 requests for one visit)
    snap = snapshot()
    rc, out, err = run_gospider(["--robots=false", "-d", "2",
                                 "--request-budget", "3"], [site_a + "/rstart"])
    sums = summaries(out)
    hits_a = hits_since("A", snap["A"])
    check("redirect: budget 3 used by root + 2 hops",
          len(sums) == 1 and sums[0]["used"] == 3, f"{sums}")
    check("redirect: budget 3 truncated mid-chain",
          sums and sums[0]["truncated"] is True, f"{sums}")
    check("redirect: server saw exactly root + 2 hops",
          sorted(hits_a) == ["/r2", "/redir", "/rstart"], f"{hits_a}")

    snap = snapshot()
    rc, out, err = run_gospider(["--robots=false", "-d", "2",
                                 "--request-budget", "4"], [site_a + "/rstart"])
    sums = summaries(out)
    hits_a = hits_since("A", snap["A"])
    check("redirect: budget 4 covers the whole chain",
          len(sums) == 1 and sums[0]["used"] == 4, f"{sums}")
    check("redirect: budget 4 not truncated (used == budget but crawl finished)",
          sums and sums[0]["truncated"] is False, f"{sums}")
    check("redirect: server saw the full chain",
          sorted(hits_a) == ["/r2", "/r3", "/redir", "/rstart"], f"{hits_a}")

    # ------------------------------------------------------------------
    # Case 6: failed attempts are charged too
    # ------------------------------------------------------------------
    # -B disables robots/linkfinder-processing; the [src] JS file is still
    # downloaded. Expected attempts: /, /p1, /p2, /p3, /redir, /r2, /r3,
    # /app.js (8 server-visible) + 1 dead-port failure = 9 used.
    snap = snapshot()
    rc, out, err = run_gospider(["-B", "--robots=false", "-d", "3",
                                 "--request-budget", "50"], [site_a])
    sums = summaries(out)
    hits_a = hits_since("A", snap["A"])
    expected_hits = sorted(["/", "/p1", "/p2", "/p3", "/redir", "/r2", "/r3", "/app.js"])
    check("failures: server-visible requests match expectation",
          sorted(hits_a) == expected_hits,
          f"extra={sorted(set(hits_a) - set(expected_hits))} "
          f"missing={sorted(set(expected_hits) - set(hits_a))}")
    check("failures: used = server hits + 1 failed attempt",
          len(sums) == 1 and sums[0]["used"] == len(hits_a) + 1 == 9, f"{sums}")
    check("failures: crawl completed without truncation",
          sums and sums[0]["truncated"] is False, f"{sums}")

    # ------------------------------------------------------------------
    # Case 7: quiet mode - stdout stays URL-only, summary on stderr
    # ------------------------------------------------------------------
    rc, out, err = run_gospider(["-q", "-d", "2", "--request-budget", "2"], [site_a])
    check("quiet: stdout lines are URLs only",
          all(re.match(r"^https?://", line) for line in out.splitlines()),
          f"stdout={out!r}")
    check("quiet: no summary on stdout", summaries(out) == [])
    sums = summaries(err)
    check("quiet: JSON summary on stderr",
          len(sums) == 1 and sums[0]["used"] == 2 and sums[0]["truncated"] is True,
          f"stderr={err!r}")

    # ------------------------------------------------------------------
    # Case 8: json mode - every stdout line parses, summary included
    # ------------------------------------------------------------------
    rc, out, err = run_gospider(["--json", "-d", "2", "--request-budget", "2"], [site_a])
    lines = [l for l in out.splitlines() if l.strip()]
    parsed = []
    try:
        parsed = [json.loads(l) for l in lines]
        ok = True
    except json.JSONDecodeError:
        ok = False
    check("json: every stdout line is valid JSON", ok, f"stdout={out!r}")
    budget_lines = [p for p in parsed if p.get("type") == "request-budget"]
    check("json: exactly one budget summary line",
          len(budget_lines) == 1 and budget_lines[0]["used"] == 2, f"{budget_lines}")

    # ------------------------------------------------------------------
    print()
    if FAILURES:
        print(f"{len(FAILURES)} FAILURE(S):")
        for f in FAILURES:
            print("  -", f)
        sys.exit(1)
    print("ALL CHECKS PASSED")
    sys.exit(0)


if __name__ == "__main__":
    main()
