# Design

## Scope

The container owns four runtime surfaces:

- `cloudflare-web`: HTTP API, static GUI, process lifecycle, candidate cache and active-pool commit.
- `cfdata`: background IP discovery scanner, writes `ip.csv` cache. Decoupled from optimizer.
- `cfnat`: local TCP forwarding through the selected fixed or scanned IP pool. It performs no protocol probe and binds one client TCP connection to one upstream IP; fallback is sequential only when dialing fails.
- `sing-box`: short-lived VLESS data-plane probes; it is not a persistent proxy service.

The Web layer orchestrates these binaries. It does not duplicate their scanning algorithms.

## Three-Pool Model

The optimizer maintains three pools with different test costs and lifecycles:

### 1. Candidate Pool (候选池)
- **Sources**: User IPs > Subscription Feed > CFdata Cache (manual-only) > Official CF CIDR Sampling
- **Test**: Low-cost TCP/TLS latency screening
- **Size**: Large (hundreds to thousands)
- **Purpose**: Fast discovery of potentially usable IPs
- Preferred domains are NOT a candidate source anymore: fallback-only, no resolve/probe.

### 2. Standby Pool (替补池)
- **Source**: Candidate IPs that pass TCP screening
- **Test**: Full VLESS quality testing (latency + speed + stability)
- **Size**: Medium (dozens)
- **Purpose**: Pre-validated, waiting for promotion opportunity

### 3. Active Pool (工作池)
- **Source**: Standby IPs that prove consistently superior
- **Test**: Passive connection metrics only (no active speed tests to avoid disrupting service)
- **Size**: Minimal (1-5)
- **Purpose**: Actual traffic forwarding

### Promotion Flow

```
Candidate Pool (hundreds)
    │
    │ TCP/TLS low-cost screening
    ▼
Standby Pool (dozens)
    │
    │ VLESS full quality test
    │ Consecutive 3 rounds proving superior
    │ Min 30min since last switch
    │ + global pool-switch cooldown (30min)
    │ + active pool health gate (all-healthy keeps pool)
    ▼
Active Pool (1-5)
    │
    │ Forward user traffic
    │ Not probed by background optimizer
    │ Passive health only (success/no-recent-failure)
```

### Promotion Threshold

- **RequiredSuperiorRounds**: 3 (must prove superior for 3 consecutive rounds)
- **RelativePromotionGain**: 25% (speed must improve by ≥25%)
- **AbsolutePromotionGainMbps**: 80 (and by ≥80Mbps absolute)
- **FailureThreshold**: 3 (3 consecutive failures before a line is failed; single jitter no longer flips the pool)
- **MinimumSwitchInterval**: 30 minutes (prevent oscillation)
- **Global pool-switch cooldown**: 30 minutes (`PROXY_POOL_SWITCH_COOLDOWN_MINUTES`), applies to both scheduler and full auto-apply; manual apply is exempt
- **Active health gate**: if every active IP has successes and no failure inside the health window (`PROXY_ACTIVE_HEALTH_WINDOW_MINUTES`, default 60), automatic switches are refused
- **Same-pool skip**: identical IP sets never restart cfnat

When a Standby IP is promoted, the old Active IP demotes to Standby (preserved for future promotion).

## Source Priority

Source priority determines **when** IPs are tested, not their final performance ranking:

1. **User** (manual IPs, `PROXY_USER_CANDIDATES`, manual apply) - Tested first, highest priority
2. **Subscription** (daily third-party maintainer feed, WS-verified into `subscription-pool.json`) - Tested second
3. **CFdata** (manual-only scan cache) - Tested third
4. **Official** (CF CIDR sampling) - Fallback expansion only
5. **Preferred domains** - NOT tested, NOT ranked; fallback-only forwarding (`-fallback`), tried only when the whole IP pool fails to dial

The actual Active Pool selection is based on measured performance (Mbps, latency, stability), not source priority.

## CFdata Manual-Only

CFdata is a **manual IP discovery scanner**, with no background self-start of any kind:

```
User (GUI "run scan" / full optimization / /api/cfdata/run)
    │
    │ Explicit trigger only
    ▼
ip.csv (cache)
    │
    │ Read-only access
    ▼
Optimizer
```

- There is no background loop, no periodic scan, no post-scan sift (`PROXY_CFDATA_BACKGROUND_*` / `PROXY_CFDATA_SIFT_COUNT` are deprecated and unread)
- Optimizer reads from cache via `cfdataCandidateIPs()` - never triggers CFdata scan

## Candidate Pipeline

Candidate generation and business acceptance are separate stages:

1. CFdata reads the local broad CDN ranges and writes `ip.csv` (manual trigger only).
2. The candidate cache preserves source order: user-supplied IPs, subscription-feed IPs, CFdata output, then sampled official Cloudflare CIDRs. Preferred domains never enter the candidate IP list.
3. The first active-pool stage validates the configured TLS SNI, HTTP Host and WebSocket path in parallel.
4. If VLESS probing is enabled, WS passes are tested sequentially through a short-lived sing-box process. Each pass must complete both the configured `generate_204` request and a bounded download through the same tunnel.
5. The VLESS probe replaces only the candidate server and port; UUID, TLS/ECH/uTLS and WebSocket settings come from the local outbound template. Results preserve source priority and use measured Mbps, then data-plane latency, within each source tier.
6. A new pool is committed only when it meets the configured minimum size and CFnat starts successfully.
7. Any probe, template or process failure leaves the previous active pool and cache in place.

`proxy-candidates.json` is discovery state. `proxy-active.json` is last-known-good forwarding state. They must not be treated as interchangeable.

The CFnat data plane never launches these probes. It assigns one upstream target to each new TCP connection using strict round-robin, keeps that session pinned, and only attempts the next target after a TCP dial failure. The fixed pool holds public IPv4 only; preferred domains live exclusively in `-fallback` and are tried only when every fixed IP fails to dial. Domain targets resolve their current DNS (and therefore the operator's current preferred IP) at dial time, under the same session-pinning semantics.

## Active Isolation

The background optimizer **never probes Active IPs**:

- `runQualitySchedulerProbe()` filters out Active IPs from the candidate batch
- Active health is monitored only through passive connection metrics (success rate, failure rate)
- This ensures background optimization does not disrupt active proxy connections

## Web GUI

The GUI is a functional operations console:

- The overview prioritizes active pool, candidate count, process state, the next refresh and one full-pipeline action.
- Common CFnat and CFdata settings remain visible; all original command flags remain available under advanced sections.
- Manual candidate scanning remains separate from the WebSocket business probe because a TCP/TLS pass is not proof of node usability.
- Logs are one shared workspace with process selection, search, level filtering, line limits, pause, follow and copy controls.
- Buttons expose pending and disabled states so repeated clicks cannot fan out duplicate browser requests.

The "完整优选" browser action runs CFdata, waits for a successful exit, refreshes merged candidates, runs the server-side WebSocket final probe, and then renders the returned active pool as the authoritative result. The existing six-hour server scheduler remains the unattended path if the browser closes.

## Failure Boundaries

- User-triggered stop actions require confirmation.
- Long-running actions report their current stage and restore button state on both success and failure.
- External candidate sources are server allowlisted; arbitrary remote scripts or URLs are not accepted.
- The VLESS outbound template lives under `/data`, is mode `0600` when copied by the operator, and is never returned by the API. Temporary sing-box configs are mode `0600`, redacted on errors and deleted after each probe.
- All rendered log and result content is escaped before insertion into HTML.
- Mobile tables scroll inside their result surface and cannot widen the document viewport.

## Environment Variables

### Subscription Feed (daily IP supply)
- `PROXY_SUBSCRIPTION_CONVERTER`: Subscription converter base URL (default: `https://vlesdy.trojanjd.dpdns.org/sub`); requests are `?token=…&sub=<maintainer>`
- `PROXY_SUBSCRIPTION_MAINTAINERS`: Comma/space-separated third-party maintainer hosts (default: 8 maintainers incl. `owo.o00o.ooo`, `cm.soso.edu.kg`, `zrf.zrf.me`)
- `PROXY_SUBSCRIPTION_TOKEN`: Converter token, injected from `.env`, never committed
- `PROXY_SUBSCRIPTION_REFRESH_ENABLED`: Enable the daily slow sweep (default: true)
- `PROXY_SUBSCRIPTION_REFRESH_MINUTES`: Sweep interval in minutes (default: 360, min: 30)
- `PROXY_SUBSCRIPTION_PER_MAINTAINER`: Max DNS-resolved IPs kept per maintainer (default: 30)

### Pool-Switch Anti-Flapping
- `PROXY_POOL_SWITCH_COOLDOWN_MINUTES`: Global cooldown between automatic pool switches (default: 30, min: 5); manual apply is exempt
- `PROXY_ACTIVE_HEALTH_WINDOW_MINUTES`: Active pool health window (default: 60, min: 10); an all-healthy pool blocks automatic switches

### CFdata (manual-only; background vars deprecated)
- `PROXY_CFDATA_BACKGROUND_ENABLED/MINUTES/TIMEOUT`, `PROXY_CFDATA_SIFT_COUNT`: no longer read; CFdata runs only on explicit user trigger

### Preferred Domains (fallback-only)
- `PROXY_PREFERRED_MAX_DOMAINS`: compat only (default: 20); domains are never resolved/probed into the candidate pool
- `PROXY_PREFERRED_PROBE_MINUTES`, `PROXY_DYNAMIC_DISCOVERY`: deprecated, no longer read

### Quality Scheduler
- `PROXY_SCHEDULER_APPLY`: Enable automatic pool switching (default: true)
- `PROXY_SCHEDULER_PROBE_INTERVAL_SECONDS`: Probe interval in seconds (default: 300)
- `PROXY_SCHEDULER_BATCH_SIZE`: IPs per probe batch (default: 12)
