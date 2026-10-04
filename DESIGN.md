# Design

## Scope

The container owns four runtime surfaces:

- `cloudflare-web`: HTTP API, static GUI, process lifecycle, candidate cache and active-pool commit.
- `cfdata`: manual IP discovery scanner, writes the `ip.csv` cache. It is decoupled from the optimizer and has no background loop.
- `cfnat`: local TCP forwarding through the selected fixed or scanned IP pool. It performs no protocol probe and binds one client TCP connection to one upstream IP; fallback is sequential only when dialing fails.
- `sing-box`: short-lived VLESS data-plane probes; it is not a persistent proxy service.

The Web layer orchestrates these binaries. It does not duplicate their scanning algorithms.

## Three-Pool Model

The optimizer maintains three pools with different test costs and lifecycles:

### 1. Candidate Pool (候选池)
- **Sources**: User IPs > Subscription Feed > Preferred-domain resolution > CFdata Cache > Official CF CIDR Sampling > enabled community sources
- **Test**: Low-cost TCP/TLS latency screening
- **Size**: Large (hundreds to thousands)
- **Purpose**: Fast discovery of potentially usable IPs
- Preferred domains now do double duty: each candidate refresh resolves enabled domains into public IPv4 (C-zone supply, capped per-domain and globally, snapshot expires with the refresh cycle), while remaining in `-fallback` for dial-time DNS-based forwarding.

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
    │ Consecutive 3 rounds proving superior (scheduler only)
    │ Min 30min since last switch
    │ + global pool-switch cooldown (30min)
    │ + active pool health gate (all-healthy keeps pool)
    ▼
Active Pool (1-5)
    │
    │ Forward user traffic
    │ Excluded from candidate rotation
    │ Short WS health probe keeps the switch guard current
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
3. **Preferred resolved** (C-zone: enabled preferred domains resolved per refresh, snapshot expires with the cycle) - Tested third; supersedes the upstream author's IP supply position
4. **CFdata** (manual-only scan cache) - Tested fourth, demoted to manual fallback supply
5. **Official** (CF CIDR sampling) - Fallback expansion only
6. **Community** (enabled allowlisted DNS/API sources) - Last expansion layer
7. **Preferred domains (raw)** - remain untested as domains; they keep `-fallback` duty only. Resolved IPs from the same domains are tested through the normal pipeline.

The actual Active Pool selection is based on measured performance (Mbps, latency, stability), not source priority.

## CFdata Manual-Only

CFdata is a **manual IP discovery scanner**, with no background self-start of any kind:

```
User (explicit /api/cfdata/run request)
    │
    │ Explicit trigger only
    ▼
ip.csv (cache)
    │
    │ Read-only access
    ▼
Optimizer
```

- There is no background loop, no periodic scan, and no post-scan sift; the optimizer only reads the existing cache.
- Optimizer reads from cache via `cfdataCandidateIPs()` - never triggers CFdata scan

## Candidate Pipeline

Candidate generation and business acceptance are separate stages:

1. CFdata reads the local broad CDN ranges and writes `ip.csv` (manual trigger only).
2. The candidate cache preserves source order: user-supplied IPs, subscription-feed IPs, preferred-domain resolved IPs (C-zone snapshot, re-resolved each refresh), CFdata output, sampled official Cloudflare CIDRs, then enabled community sources.
3. The first active-pool stage validates the configured TLS SNI, HTTP Host and WebSocket path in parallel.
4. If VLESS probing is enabled, WS passes are tested sequentially through a short-lived sing-box process. Each pass must complete both the configured `generate_204` request and a bounded download through the same tunnel.
5. The VLESS probe replaces only the candidate server and port; UUID, TLS/ECH/uTLS and WebSocket settings come from the local outbound template. Results preserve source priority and use measured Mbps, then data-plane latency, within each source tier.
6. A new pool is committed only when it meets the configured minimum size and CFnat starts successfully.
7. Any probe, template or process failure leaves the previous active pool and cache in place.

`proxy-candidates.json` is discovery state. `proxy-active.json` is last-known-good forwarding state. They must not be treated as interchangeable.

The CFnat data plane never launches these probes. It assigns one upstream target to each new TCP connection using strict round-robin, keeps that session pinned, and only attempts the next target after a TCP dial failure. The fixed pool holds public IPv4 only; preferred domains live exclusively in `-fallback` and are tried only when every fixed IP fails to dial. Domain targets resolve their current DNS (and therefore the operator's current preferred IP) at dial time, under the same session-pinning semantics.

## Active Isolation

The background optimizer never includes Active IPs in the candidate rotation. It
does run a short WS health probe for the current pool so the health gate has
current evidence without changing existing CFnat sessions:

- `runQualitySchedulerProbe()` filters out Active IPs from the candidate batch
- Active health is recorded separately from candidate quality records
- This ensures background optimization does not disrupt active proxy connections

## Web GUI

The GUI is a single-page console ("优选台"). Candidate refresh and subscription maintenance run unattended; automatic active-pool replacement is controlled by `PROXY_AUTO_APPLY`. The user has exactly three actions:

- **Observe**: one hero card lists the active pool — per-IP live connection count (from `/api/cfnat/upstreams`, counting only entries marked `inPool`, grouped by remote:CFNAT_PORT, active TCP states only), VLESS exam scores (Mbps/latency) and source badges. A pulse line shows whether each supply tier (manual / subscription / preferred-resolve / cfdata / official / community) currently contributes candidates.
- **Optimize**: one button triggers the full pipeline (refresh candidates → WS screening → optional VLESS final → commit when auto apply is enabled). A paste box performs the explicit manual TCP/TLS screening path and applies passing IPs immediately; background scheduling can continue the full WS/VLESS review. Manual supply has highest priority and is exempt from pool-switch cooldown.
- **Kick**: per-IP "踢" removes a pool member on user demand — the pool restarts with the remaining IPs (manual path, no cooldown), and the kicked IP is dropped from the candidate cache so the background does not immediately re-elect it. The last pool member cannot be kicked.

Logs live in a collapsed drawer. There are no routes, no config forms, no advanced panels; cfnat/cfdata flags come from the environment only. The old multi-page console (overview/forwarding/arena/scan/files) is retired.

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

### Preferred Domains (resolve-supply + fallback)
- `PROXY_PREFERRED_MAX_DOMAINS`: compat only (default: 20); cap on domains participating in resolve-supply and fallback
- `PROXY_PREFERRED_RESOLVE`: enable C-zone resolve-supply (default: true)
- `PROXY_PREFERRED_IPS_PER_DOMAIN`: max public IPv4 kept per resolved domain (default: 4)
- `PROXY_PREFERRED_RESOLVED_CANDIDATES`: global cap on resolved-supply IPs; `0` disables supply (default: 64)
- `PROXY_PREFERRED_PROBE_MINUTES`, `PROXY_DYNAMIC_DISCOVERY`: deprecated, no longer read

### Quality Scheduler
- `PROXY_SCHEDULER_APPLY`: Enable automatic pool switching (default: true)
- `PROXY_SCHEDULER_PROBE_INTERVAL_SECONDS`: Probe interval in seconds (default: 300)
- `PROXY_SCHEDULER_BATCH_SIZE`: IPs per probe batch (default: 12)

### Candidate Auto-Apply and VLESS Probe
- `PROXY_AUTO_APPLY`: Enable the full candidate WS screening and active-pool replacement (default: false when running the Web binary directly; Compose sets it explicitly).
- `PROXY_AUTO_HOST`, `PROXY_AUTO_PATH`, `PROXY_AUTO_PORT`: Host/SNI, WebSocket path, and candidate port used for screening.
- `PROXY_AUTO_CONCURRENCY`, `PROXY_AUTO_MAX_LATENCY`: WS screening concurrency and per-IP timeout.
- `PROXY_AUTO_POOL_SIZE`, `PROXY_AUTO_MIN_POOL`: maximum active-pool size and minimum passing count required for replacement.
- `PROXY_VLESS_PROBE`: Enable the optional sing-box data-plane stage (default: false). It is independent from candidate refresh; when enabled, the local template and sing-box binary settings are also required.
