# `--request-budget` regression

Local two-site regression for the `--request-budget` flag.

## What it covers

`regression.py` starts two local HTTP sites with known routes — HTML links
(multi-entry crawling), a JS file processed by linkfinder, `robots.txt`,
`sitemap.xml` (plus the 11 probed alternates), a 3-hop redirect chain, a
dead link (failed attempt) and an extension-filtered link — and then checks,
against the requests the servers actually observed:

1. **Negative budget** is rejected before startup and sends zero requests.
2. **Budget 0** keeps the original behavior: no summary output, full crawl,
   exact expected request set on both sites, dedup and extension filtering
   unchanged.
3. **Budget 1** with two sites crawled in parallel: each site spends exactly
   1 attempt (budgets are independent), is reported truncated, and exactly
   one request reaches each server.
4. **Budget 7** under `-c 8` concurrency: each site spends exactly 7, the
   servers never observe more than 7 requests per site (no overspend).
5. **Redirects**: a 3-hop chain charges every hop; budget 3 truncates
   mid-chain, budget 4 completes it with `truncated: false` even though
   `used == budget`.
6. **Failures**: a connection-refused attempt is charged
   (`used == server-observed + 1`) without truncating the crawl.
7. **Quiet mode**: stdout stays URL-only; the JSON summary goes to stderr.
8. **JSON mode**: every stdout line stays parseable; exactly one
   `request-budget` summary line is emitted per site.

## Running

```sh
go build -o gospider .
python3 tests/request_budget/regression.py ./gospider
```

The script is self-contained (Python 3 standard library only) and exits
non-zero if any check fails. If your environment routes local traffic
through a proxy, the script already pins `NO_PROXY=127.0.0.1,localhost`
for the gospider subprocess.
