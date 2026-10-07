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
- **Sources**: User IPs > Subscription Feed > bestcf tables (each
  layer quota-capped; see Source Quotas)
- **Test**: Low-cost TCP/TLS latency screening
- **Size**: Large (hundreds to thousands)
- **Purpose**: Fast discovery of potentially usable IPs
- Preferred domains are not resolved into candidates: they are promoted directly to cfnat `-fixed` members (dial-time live DNS, `applyPoolLocked`) and keep `-fallback` duty when explicitly configured. No snapshot, no domain-level probing, no candidate-pool membership.

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
    │ Dual-track superiority (superiorTo): throughput when both
    │ sides have speed data, else WS latency + success rate
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
    │   AND builds the Active latency baseline for the WS track
```

### Promotion Threshold

Superiority is judged by a single function `superiorTo(candidate, active, policy)` with **two mutually exclusive tracks**:

- **Track A — throughput** (both sides have valid throughput samples, i.e. `avg > 0`): candidate must beat active by **≥25% relative AND ≥80 Mbps absolute**.
- **Track B — WS metrics** (either side lacks throughput — VLESS probe off, speed samples sparse, or Active never speed-tested): candidate must beat active on **WS latency by ≥25% relative AND ≥15 ms absolute**, with **success rate no worse than Active's**. If either side has no latency baseline yet, the round is simply not judged superior (no blind switching).

> **`Mbps = 0` does not mean "0 Mbps measured".** It means *the sample carries no valid throughput evidence* (not speed-tested, or speed test failed). Averages are computed only over samples with `Mbps > 0`. Never compare two zero averages with an additive threshold — `0 >= 0 + 80` is always false and silently deadlocks promotion. That exact bug produced `SUPERIOR_GT0 = 0 / 9106` records in production before this design.

Active builds its WS baseline from the per-round lightweight health probe (latency + outcome recorded every scheduler round), so Track B has a judge with evidence within one probe interval of any seed.

Other thresholds:

- **RequiredSuperiorRounds**: 3 (must prove superior for 3 consecutive rounds)
- **FailureThreshold**: 3 (3 consecutive failures before a line is failed; single jitter no longer flips the pool)
- **MinimumSwitchInterval**: 30 minutes (prevent oscillation)
- **Global pool-switch cooldown**: 30 minutes (`PROXY_POOL_SWITCH_COOLDOWN_MINUTES`), applies to both scheduler and full auto-apply; manual apply is exempt
- **Understaffed exemption**: when the pool shrinks below `PROXY_AUTO_MIN_POOL` (3), the cooldown no longer blocks switches (logged as waived, but not enforced) — backfill outranks anti-flapping. Added after four rapid manual kicks (5→1 IPs) left two passing full exams discarded by cooldown.
- **Active health gate**: if every active IP has successes and no failure inside the health window (`PROXY_ACTIVE_HEALTH_WINDOW_MINUTES`, default 60), automatic switches are refused. NOTE: this gate does not consider pool size — a healthy single-member pool is still blocked from growing by the automatic path; growth then depends on the manual-apply path (exempt) or the kick-refill queue. Understaffed growth after cooldown waiver replaces the pool outright.
- **Same-pool skip**: identical IP sets never restart cfnat
- **Silent-exit audit rule**: any auto-apply exit that discards a passing exam MUST surface a message in the active-pool `Error` field (`setProxyPoolError`), not only in container logs. Exits that previously lied by silence (snapshot-changed abort, gate blockage) now report "保留旧池" with the reason, so the GUI toast reflects reality instead of a false "优选完成".
- **Kick-refill queueing**: `refillPoolAfterKick` waits up to 3 minutes for an in-flight exam instead of silently yielding. It is a one-shot goroutine per kick — a silent yield loses the backfill forever, because the scheduler only *replaces* members and never grows the pool.

When a Standby IP is promoted, the old Active IP demotes to Standby (preserved for future promotion).

## Source Priority

Source priority determines **when** IPs are tested (ingestion order on ties), not their final performance ranking:

1. **User** (manual IPs, `PROXY_USER_CANDIDATES`, manual apply) - Tested first, highest priority
2. **Subscription** (third-party maintainer feed, WS-verified into `subscription-pool.json`) - Tested second
3. **bestcf** (the merged bestcf table: IP lists, ISP feeds, and mirrors) - the expansion layer

Preferred domains are not a candidate source: they are promoted directly to cfnat `-fixed` members with dial-time DNS. CFdata is a manual push source only. The official Cloudflare CIDR sampling layer was **removed** (user directive) — `PROXY_OFFICIAL_CANDIDATES` is no longer read.

### Source Quotas (anti-monopoly)

Priority alone does not stop one source from eating the whole pool — production once showed `subscription 1000/1000, official 0, proxy 0`. Each refresh therefore applies per-source quotas (ingestion still runs in priority order, so ties go to the higher tier):

| Layer | Quota | Constant |
|---|---|---|
| user | ≤ 100 | `userCandidatesCap` |
| subscription | ≤ 600 | `subscriptionCandidatesCap` |
| bestcf | ≥ 300 floor, absorbs remaining capacity | `bestcfCandidatesFloor` |
| **total** | **≤ 1000** | `candidatePoolTotalCap` |

`subscriptionQuota = min(600, total - used - bestcfFloor)` makes it mathematically impossible for the subscription layer to starve the bestcf floor.

The actual Active Pool selection is based on measured performance (Mbps, latency, stability), not source priority.

## Repository Layout

The repository root is a **set of single-file `main` programs**, not a buildable package:

| File | Built by | Notes |
|---|---|---|
| `cfnat.go` | `go build cfnat.go` | the forwarder; `cfnat_test.go` runs via `go test cfnat.go cfnat_test.go` |
| `cfdata.go` | `go build cfdata.go` | manual IP discovery scanner |
| `x-tunnel.go` | never | **disabled** — depends on uuid / gorilla-websocket / xtaci/smux, which are absent from `go.mod` |

All root files carry `//go:build ignore`, because otherwise `go build ./...` fails on duplicate `main`/`location` declarations (and on `x-tunnel.go`'s missing imports). `go build ./...` therefore only compiles `./web`, which is where the web console lives.

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
2. The candidate cache preserves source order under per-source quotas: user-supplied IPs (≤100), subscription-feed IPs (≤600 with floor reservation), sampled official Cloudflare CIDRs (≥150 floor), then enabled community sources (≥150 floor); total ≤1000.
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

### Pool Entry Latency Ceiling
- `PROXY_POOL_MAX_ENTRY_LATENCY_MS`: WS-handshake latency ceiling for **entering** the active pool (default: 800, clamped to 50..5000). Applies to manual promote, kick-refill, and scheduler replacement alike.

A WS handshake that completes is not the same as a fast IP. Production measured on the NAS against the same SNI: good members `connect 59-64ms / WS ~190ms`, the official anycast baseline `connect 155ms`, and two members that passed every handshake check yet ran `connect 231ms` and `connect 1168ms`. Because device stickiness pins one LAN device to one upstream for long stretches, a single slow member does not merely degrade the pool — it pins that device to a 1.1-second path, and YouTube comments stop loading. So entry now requires both **reachable** and **fast**; refill aborts rather than fill with a slow IP, leaving the pool short instead of poisoned.

### CFdata (manual-only; background vars deprecated)
- `PROXY_CFDATA_BACKGROUND_ENABLED/MINUTES/TIMEOUT`, `PROXY_CFDATA_SIFT_COUNT`: no longer read; CFdata runs only on explicit user trigger

### Preferred Domains (fixed-member dial + optional fallback)
- `PROXY_PREFERRED_MAX_DOMAINS`: cap on enabled preferred domains (default: 20). The hard code constant is `maxForwardDomains = 12`.
- `PROXY_PREFERRED_RESOLVE`, `PROXY_PREFERRED_IPS_PER_DOMAIN`, `PROXY_PREFERRED_RESOLVED_CANDIDATES`: deprecated, no longer read (C-zone resolve-supply was removed; domains dial as `-fixed` members with live DNS)
- `PROXY_PREFERRED_PROBE_MINUTES`, `PROXY_DYNAMIC_DISCOVERY`: deprecated, no longer read

### VLESS Data-Plane Probe
- `PROXY_VLESS_PROBE`: enable the real sing-box data-plane check (default: false)
- `PROXY_VLESS_TEST_URL`: liveness URL through the tunnel (default: `https://www.gstatic.com/generate_204`, expects 204)
- `PROXY_VLESS_SPEED_URL`: **speed-test download endpoint** (default: `https://speed.cloudflare.com/__down?bytes={bytes}`). The `{bytes}` placeholder is substituted with `PROXY_VLESS_SPEED_BYTES`. Override this when the tunnel cannot finish the default endpoint inside `PROXY_VLESS_SPEED_TIMEOUT` — measured from production, `speed.cloudflare.com` is reachable directly (~5.5 Mbps) but routinely exceeds the budget through the VLESS→WS→TLS tunnel.

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
