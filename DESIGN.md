# Design

## Scope

The container owns four runtime surfaces:

- `cloudflare-web`: HTTP API, static GUI, process lifecycle, candidate cache and active-pool commit.
- `cfdata`: candidate discovery, data-center aggregation, latency/loss detail tests and CSV output.
- `cfnat`: local TCP forwarding through the selected fixed or scanned IP pool.
- `sing-box`: short-lived VLESS data-plane probes; it is not a persistent proxy service.

The Web layer orchestrates these binaries. It does not duplicate their scanning algorithms.

## Candidate Pipeline

Candidate generation and business acceptance are separate stages:

1. CFdata reads the local broad CDN ranges and writes `ip.csv`.
2. The candidate cache merges allowlisted community DNS pools, bounded HTTPS IP pages, CFdata output and sampled official Cloudflare CIDRs. HTTPS pages are fetched without script execution, capped at 512 KiB and cannot follow cross-origin redirects. Subscription converter frontends are not automatic sources; manually pasted output accepts hostnames only from recognized node URIs or server fields, capped at 64 names under a shared three-second deadline.
3. The first active-pool stage validates the configured TLS SNI, HTTP Host and WebSocket path in parallel.
4. If VLESS probing is enabled, only the fastest WS passes are tested sequentially through a short-lived sing-box process until the target pool is full.
5. The VLESS probe replaces only the candidate server and port; UUID, TLS/ECH/uTLS and WebSocket settings come from the local outbound template. A configured `generate_204` response is the business acceptance signal.
6. A new pool is committed only when it meets the configured minimum size and CFnat starts successfully.
7. Any probe, template or process failure leaves the previous active pool and cache in place.

`proxy-candidates.json` is discovery state. `proxy-active.json` is last-known-good forwarding state. They must not be treated as interchangeable.

## Web GUI

The GUI is a functional operations console:

- The overview prioritizes active pool, candidate count, process state, the next refresh and one full-pipeline action.
- Common CFnat and CFdata settings remain visible; all original command flags remain available under advanced sections.
- Manual candidate scanning remains separate from the WebSocket business probe because a TCP/TLS pass is not proof of node usability.
- Logs are one shared workspace with process selection, search, level filtering, line limits, pause, follow and copy controls.
- Buttons expose pending and disabled states so repeated clicks cannot fan out duplicate browser requests.

The “完整优选” browser action runs CFdata, waits for a successful exit, refreshes merged candidates, runs the server-side WebSocket final probe, and then renders the returned active pool as the authoritative result. The existing six-hour server scheduler remains the unattended path if the browser closes.

## Failure Boundaries

- User-triggered stop actions require confirmation.
- Long-running actions report their current stage and restore button state on both success and failure.
- External candidate sources are server allowlisted; arbitrary remote scripts or URLs are not accepted.
- The VLESS outbound template lives under `/data`, is mode `0600` when copied by the operator, and is never returned by the API. Temporary sing-box configs are mode `0600`, redacted on errors and deleted after each probe.
- All rendered log and result content is escaped before insertion into HTML.
- Mobile tables scroll inside their result surface and cannot widen the document viewport.
