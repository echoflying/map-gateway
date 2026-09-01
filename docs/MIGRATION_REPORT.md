# map-gateway Migration Report

- **Date:** 2026-08-14
- **Status:** Code complete, tests green (`go test ./...` all pass). **nginx cutover NOT yet performed** — see [nginx cutover pending](#nginx-cutover-pending). This service is not deployed, not restarted, and no other project was touched.

## Exact files

| File | Change |
| --- | --- |
| `main.go` | Route validation fix: tile URLs are path-only; any query string is rejected with `404 Not Found` **before** rate limiting, cache lookup and upstream fetch. Added `tilePathOnly(r)` helper; guard installed at the top of `handleTile` and `handleTianditu`. Header docs updated. |
| `main_test.go` | `TestBadPathsRejected` aligned with the path-only contract (see [Validation](#validation)): query-string cases moved into the 404 list, upstream-hit counter asserts rejected requests never reach upstream, and a hermetic stub upstream verifies a valid path-only URL is still served. All existing path-safety assertions preserved. |
| `docs/MIGRATION_REPORT.md` | This report. |

No other files changed; no deployment, service restart, nginx edit, or change outside this directory.

## Route ownership and compatibility

Owned by map-gateway (GET only, browser-compatible):

| Route | Provider | Notes |
| --- | --- | --- |
| `/health`, `/healthz` | — | `200 ok`, `Cache-Control: no-store` (probes / systemd). |
| `/tile/terrain/{z}/{x}/{y}.png` | AWS Terrarium elevation tiles (`s3.amazonaws.com/elevation-tiles-prod/terrarium`) | |
| `/tile/sat/{z}/{x}/{y}.jpg|.png` | ArcGIS World_Imagery; upstream order translated to `/tile/{z}/{y}/{x}` | `.png` variant is accepted and content-sniffed (a sat tile cached as `.png` may still be JPEG bytes). |
| `/tianditu/{vec\|cva\|img\|cia}/{z}/{x}/{y}.png` | Tianditu WMTS (needs `TIANDITU_KEY`) | `tk` is appended server-side only; no key ⇒ `503`. |

The rest of the webapp (everything not under `/tile/` or `/tianditu/`) stays on `webapp:8080`; nginx will route only these prefixes here (see [nginx cutover pending](#nginx-cutover-pending)). URLs served to the browser are byte-for-byte identical to the previous webapp handlers, so no front-end change is required.

**Query-string policy (new):** tile URLs are strictly path-only. Any query string (`?x=1`, `?foo=bar`, `?tk=...`) on a `/tile/` or `/tianditu/` route is rejected with `404` before rate limiting, cache, or upstream handling. Rationale: query strings carry no meaning for tile fetches, so cache-busters or injected parameters must never reach the cache or an upstream provider (and must not consume rate-limit budget). For `/tianditu/`, the query-string check runs before the key check, so an invalid route 404s even when the key is unset.

## Configuration and secret handling

- Configuration comes exclusively from the environment, optionally via an env file (`MAP_ENV_FILE`, default `./.env`). Precedence: **process env > env file > built-in default**.
- `.env` is git-ignored; `.env.example` contains placeholders only (`TIANDITU_KEY=__FILL_IN_TIANDITU_KEY__`). Copy `.env.example` → `.env`, fill in the real key.
- Relevant variables: `MAP_LISTEN_ADDR` (default `127.0.0.1:8082`, loopback — nginx proxies from there), `MAP_CACHE_DIR`, `MAP_CACHE_MAX_MB` (200), `MAP_CACHE_EVICT_WATERMARK` (0.8), `MAP_TILE_RATE` (600/min/IP), `MAP_UPSTREAM_TIMEOUT` (45s), `MAP_MAX_TILE_BYTES` (8 MiB), `TIANDITU_KEY`, and test/mirror overrides `MAP_TERRAIN_UPSTREAM`, `MAP_SAT_UPSTREAM`, `MAP_TIANDITU_UPSTREAM`.
- **Secrets:** `TIANDITU_KEY` is never exposed through any HTTP endpoint and is redacted from logs (`redact()` replaces it with `REDACTED`). Upstream URLs are never logged (the Tianditu upstream URL embeds `tk=`). Request paths never contain secrets.

## nginx cutover pending

Not yet performed — the change is code-only in this directory. When ready, nginx (outside this project's scope) must be edited to:

1. Route `location ^~ /tile/` and `location ^~ /tianditu/` to `127.0.0.1:8082` (map-gateway).
2. Pass `X-Real-IP` (from real_ip-resolved `$remote_addr`) and `X-Forwarded-For` for per-IP rate limiting; map-gateway falls back to the peer address otherwise.
3. Keep everything else on `webapp:8080`.

Do **not** cut over until validation below passes against a real nginx config and the service is supervised (systemd unit etc.).

## Cache prewarm

- Disk cache layout mirrors the old webapp: `{kind}/{z}/{x}/{y}.{ext}` under `MAP_CACHE_DIR` (default `./cache`), with `tianditu/{layer}/…` for Tianditu. An existing `~/webapp/cache` tree can be **copied** into `MAP_CACHE_DIR` as-is to prewarm; no format conversion needed.
- Cache is bounded (`MAP_CACHE_MAX_MB`, oldest-first eviction down to `MAP_CACHE_EVICT_WATERMARK`), writes are atomic (tmp + rename), and concurrent misses for the same tile are deduplicated per cache key.
- Served with `Cache-Control: public, max-age=86400`, matching the webapp semantics. Suggest prewarming the most-requested zooms before cutover to avoid an upstream burst.

## Validation

- `gofmt` clean; `go vet ./...` clean; `go test ./...` passes (all tests, including the previously failing `TestBadPathsRejected`).
- `TestBadPathsRejected` (regression suite): letters, missing extension, extra segments, wrong case/extension, zoom > 30, negatives, path traversal (`../../etc/passwd`), encoded dots, over-length coordinates, and all query-string variants on tile routes return `404`; upstream hit counter stays at 0 for rejected requests (proving rejection happens **before** cache/upstream); the valid path-only URL `/tile/terrain/3/6/1.png` is still served `200` with the tile body (behavior preserved).
- Other suites: health endpoints; unknown routes `404`; `POST` → `405` with `Allow: GET`; terrain/sat end-to-end (miss → upstream → cache → hit, no duplicate upstream fetch); ArcGIS z/y/x order; Tianditu end-to-end (incl. `tk=` in upstream query, all four layers) and no-key `503`; upstream `500` and conn-reset → `502`; oversized body rejection without caching; cache eviction oldest-first to watermark; per-IP rate limiting with `X-Real-IP`/`X-Forwarded-For`; config/env-file precedence; log redaction; content sniffing.
- Before cutover, also run a live smoke test: `curl -i 'http://127.0.0.1:8082/health'`, a valid tile URL, and a query-string URL (must be `404`).

## Rollback

- The service is standalone and does not modify any other project. Rollback = stop map-gateway and revert the nginx location blocks to the previous webapp routing; old cache tree is untouched (and remains valid for prewarm reuse).
- If the binary was deployed, redeploy the previous build (no schema/migration involved; cache format is unchanged).
- No data migration; disk cache is disposable (it is a cache — can be deleted and rebuilt, or preserved for prewarm).

## Known risks

- **Rate limiting trusts proxy headers:** `X-Real-IP`/`X-Forwarded-For` are taken at face value. nginx must be the only path to this service and must overwrite those headers from the trusted `$remote_addr`, otherwise clients could spoof IPs to bypass the 600/min/IP limit or to poison per-IP buckets.
- **Query-string rejection is a behavior change:** any existing caller that appended cache-busting params (e.g. `?v=…`) to tile URLs will now get `404`. Front-end audit recommended before cutover; this is intentional (see [Route ownership and compatibility](#route-ownership-and-compatibility)).
- **Upstream dependence:** the gateway has no fallback mirror; AWS/ArcGIS/Tianditu outages surface as `502`. `MAP_UPSTREAM_TIMEOUT` (45s) and `MAP_MAX_TILE_BYTES` (8 MiB) bound the blast radius. First-hit latency includes a full upstream fetch; prewarm mitigates.
- **Tianditu key is required for `/tianditu/*`:** if `TIANDITU_KEY` is unset/misconfigured in the deployed environment, those routes return `503` (by design, matching the webapp); terrain/sat are unaffected.
- **Eviction is O(cache size)** on the write that crosses the cap (walks and sorts the whole tree); acceptable at the 200 MiB default, worth re-checking if `MAP_CACHE_MAX_MB` grows large.
- **Single-process, in-memory rate-limit state:** counters reset on restart; a multi-replica deployment would need shared state (not currently planned; nginx routes to one instance).

---

## Deployment update — 2026-08-14 23:xx CST

**Status: deployed in the experimental environment.**

- Installed and enabled `map-gateway.service`; it is active on `127.0.0.1:8082`. The project-owned unit is `deploy/map-gateway.service`.
- The existing server-side Tianditu key was copied into this project's private `.env` without being printed or committed. A fresh `cache/` directory is in use; the former webapp cache was deliberately not prewarmed because this is experimental data.
- nginx now routes `^~ /tile/` and `^~ /tianditu/` to this service while retaining all other webapp routes. Browser URLs were unchanged.
- Verified through nginx: `/tile/terrain/3/6/2.png` and `/tianditu/vec/3/6/2.png` both returned `200`; `/health` returned `200` directly on port 8082.
- Fast cutover script: `/home/piboy/projects/kml3d/deploy/activate-fast-experimental.sh`. To roll back, restore `/etc/nginx/sites-available/piboy.fast-experimental.bak`, reload nginx, then stop/disable `map-gateway.service`.