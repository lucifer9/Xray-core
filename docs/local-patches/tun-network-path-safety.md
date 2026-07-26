# Local patch ledger: TUN network path safety

This file tracks the behavior that remains local relative to `upstream/main`. The maintained object is the residual behavior delta, not the original commit hash.

## Patch stack

Checked against: `upstream/main@5ca6f4b7d4dc20a881d4330e498892697627ec0c`

```text
upstream/main
└── main
    └── local/dial-controls       2832c088
        └── local/tun-carrier-policy  df407f2c
            └── local/route-exclusions  7b96270b
                └── local/direct-echo  e5899311
                    └── dev
```

The 2026-07-28 and 2026-07-29 syncs were accepted and their `backup/dev-before-upstream-sync-*` refs removed. Earlier pre-reorganization history is no longer preserved: the code tree at `local/direct-echo` is identical to that old history's tip; only the local commit boundaries changed.

## Upstream status vocabulary

| Status | Meaning |
|---|---|
| `local-only` | Upstream does not provide the behavior |
| `candidate` | Possible overlap has not yet been proven |
| `partial` | Upstream provides part of the behavior; retain only the residual delta |
| `absorbed` | Upstream fully provides the same patch or behavior |
| `superseded` | A different upstream design satisfies the complete behavior contract |
| `diverged` | Upstream conflicts with a required local invariant |

## Behavior units

| Issue | Behavior unit | Local branch | Local commit | Upstream status | Upstream commits | Residual delta |
|---|---|---|---|---|---|---|
| 01 | Lifecycle-scoped required dial controls | `local/dial-controls` | `2832c088` | `local-only` | — | Required controller lifecycle, stable snapshots, propagated failures, local DNS integration, and socket-option ordering |
| 02 | Process-wide Outbound carrier policy ownership | `local/tun-carrier-policy` | `df407f2c` | `local-only` | — | Single lifecycle owner, restart-safe cleanup, family isolation, fail-closed connection legs, and loopback exemption |
| 03 | Linux Outbound carrier tracking | `local/tun-carrier-policy` | `df407f2c` | `local-only` | — | Observer-before-snapshot startup, route/link refresh, and family-specific default-route selection |
| 04 | macOS Outbound carrier tracking | `local/tun-carrier-policy` | `df407f2c` | `local-only` | — | Routing-socket observation, usable-route selection, iOS limitation, and family-specific socket binding |
| 05 | Windows default-route carrier tracking | `local/tun-carrier-policy` | `df407f2c` | `local-only` | — | Route-table selection using combined metrics and route/interface change observation |
| 06 | Common CIDR Automatic system route planner | `local/route-exclusions` | `7b96270b` | `local-only` | — | Deterministic normalization, subtraction, capacity checks, fuzz coverage, and large fixture |
| 07 | Linux Excluded route prefixes | `local/route-exclusions` | `7b96270b` | `local-only` | — | Common plan installation, incremental ownership, and reverse rollback |
| 08 | macOS Excluded route prefixes | `local/route-exclusions` | `7b96270b` | `local-only` | — | Exclusions after protected-default expansion and owned-route rollback |
| 09 | Windows Excluded route prefixes | `local/route-exclusions` | `7b96270b` | `local-only` | — | Wintun routes derived from the common route plan |
| 10 | Echo prober seam with Local Echo compatibility | `local/direct-echo` | `e5899311` | `local-only` | — | One lifecycle-aware prober seam for IPv4 and IPv6 with Local Echo as the default |
| 11 | Bounded Direct Echo engine | `local/direct-echo` | `e5899311` | `local-only` | — | Shared sockets, remapping, matching, limits, carrier replacement, cancellation, and shutdown |
| 12 | Linux Direct Echo transport | `local/direct-echo` | `e5899311` | `local-only` | — | Raw ICMPv4/ICMPv6 transport bound to the Linux carrier interface |
| 13 | macOS Direct Echo transport | `local/direct-echo` | `e5899311` | `local-only` | — | Family-specific Darwin raw-socket binding and reply metadata |
| 14 | Direct Echo activation | `local/direct-echo` | `e5899311` | `local-only` | — | Public configuration, supported-platform activation, startup validation, documentation, and explicit unsupported-platform failure |

The 2026-07-28 sync reviewed upstream commits `5b1b4105` and `4aba687d`. `5b1b4105` is patch-equivalent to the previous baseline's `d291486c` process-lookup fix, and `4aba687d` changes gRPC/XHTTP local-address reporting. Neither overlaps behavior units 01–14, so all residual deltas remain `local-only`.

The 2026-07-29 sync reviewed upstream commits `6ab123bf`, `18e28390`, and `5ca6f4b7`. `6ab123bf` adds XMC finalmask directional padding under `transport/internet/finalmask/xmc`, `18e28390` reduces the XHTTP client default `maxConnections` in `infra/conf/transport_method.go`, and `5ca6f4b7` is the v26.7.28 version bump. None overlaps behavior units 01–14, so all residual deltas remain `local-only`; the patch queue was rebased verbatim (`range-diff` shows every patch unchanged).

## Verification at current baseline

Validated on 2026-07-29:

```text
go test ./transport/internet ./features/dns/localdns ./proxy/tun
go test ./infra/conf -run '^TestTunConfig'
go test -race ./transport/internet ./features/dns/localdns ./proxy/tun
GOOS=linux GOARCH=amd64 go test -exec=/usr/bin/true ./proxy/tun
GOOS=windows GOARCH=amd64 go test -exec=/usr/bin/true ./proxy/tun
GOOS=freebsd GOARCH=amd64 go test -exec=/usr/bin/true ./proxy/tun
CGO_ENABLED=0 GOOS=android GOARCH=arm64 go test -exec=/usr/bin/true ./proxy/tun
CGO_ENABLED=1 GOOS=ios GOARCH=arm64 go test -exec=/usr/bin/true ./proxy/tun
```

The complete `infra/conf` package test suite additionally requires repository geodata assets; the targeted TUN config tests pass without those external fixtures.

## Synchronization procedure

Use the project skill:

```text
/skill:upstream-sync
```

Its implementation is `.pi/skills/upstream-sync/SKILL.md`. Every successful upstream synchronization must update this ledger's checked commit, overlap statuses, upstream commit references, residual deltas, and verification evidence.
