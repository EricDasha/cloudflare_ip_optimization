# Quality Scheduler Design

## Boundary

CFnat/IP sources remain responsible for discovering candidates. Probe code remains responsible for measuring them. This module only evaluates quality records and decides whether a candidate should become Active. It has no network or process side effects.

## State Model

UNKNOWN -> PROBING -> STANDBY -> ACTIVE -> DEGRADED -> FAILED is represented by an explicit transition table. Every transition requires a StateReason and timestamp for auditability.

## Decision Rules

- PROMOTION requires a Standby candidate to exceed both the relative and absolute throughput gains for the configured number of consecutive rounds.
- PROMOTION obeys MinimumSwitchInterval.
- FAILOVER is emitted immediately when Active is failed or reaches the failure threshold; it bypasses the normal promotion cooldown.
- Scores cap throughput at the configured link capacity (default 1024 Mbps), with average speed weighted above P95, peak and success rate.

## Non-goals

This phase does not persist quality history, schedule probes, call CFnat, or change the NAS deployment. Those integrations require a later phase after the pure decision model remains stable under replayed observations.
