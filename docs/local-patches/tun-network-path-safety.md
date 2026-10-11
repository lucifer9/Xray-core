# Local patch ledger: TUN network path safety

This file tracks the behavior that remains local relative to the checked upstream mirror. The maintained object is the residual behavior delta, not the original commit hash.

## Patch stack

Checked against: `main@701af60772cda123492f86e383e1fdba066614a9` (equal to `upstream/main` at sync time).

The clone now has an `upstream` remote, and `main` mirrors `upstream/main`. The 2026-09-29 and 2026-09-30 synchronizations used the existing `main` without fetching, moving `main`, or pushing.

```text
main 701af607
└── local/dial-controls  895a031c
    └── local/tun-carrier-policy  6b4767c5
        └── local/route-exclusions  f7ad1f1d
            └── local/direct-echo  1fa18904
                └── local/panel  c07c9c6e  (see balancer-panel.md)
                    └── dev       (maintenance documentation)
```

Before the 2026-09-29 synchronization, the base was `e5e85ca9` and the integration tip was `130d133a`. The previous topic tips were `ee4bb321`, `684a1777`, `6c6b79f0`, and `eee79670`. The 2026-09-29 and 2026-09-30 syncs were accepted and their `backup/*-before-main-rebase-*` refs removed.

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

All rows below are checked against `main@701af607`. Verification and platform limitations are recorded below the table. No complete local commit was absorbed or dropped.

| Issue | Behavior unit | Local branch | Local commit | Upstream status | Upstream commits | Residual delta |
|---|---|---|---|---|---|---|
| 01 | Lifecycle-scoped required dial controls | `local/dial-controls` | `895a031c` | `local-only` | — | Required controller lifecycle, stable snapshots, propagated failures, local DNS integration, and socket-option ordering |
| 02 | Process-wide Outbound carrier policy ownership | `local/tun-carrier-policy` | `6b4767c5` | `local-only` | — | Single lifecycle owner, restart-safe cleanup, family isolation, fail-closed connection legs, and loopback exemption |
| 03 | Linux Outbound carrier tracking | `local/tun-carrier-policy` | `6b4767c5` | `local-only` | — | Observer-before-snapshot startup, route/link refresh with failure-log de-duplication, and family-specific default-route selection |
| 04 | macOS Outbound carrier tracking | `local/tun-carrier-policy` | `6b4767c5` | `local-only` | — | Routing-socket observation, usable-route selection, iOS limitation, and family-specific socket binding |
| 05 | Windows default-route carrier tracking | `local/tun-carrier-policy` | `6b4767c5` | `partial` | `c7245c03`, `1f304916`, `8989adfd` | Family-specific route selection, combined-metric ordering without Wi-Fi preference, policy ownership, observer-before-snapshot startup, and failure cleanup; required-controller integration for upstream WFP DNS resolution; upstream weak host send guard driven by the per-family carriers and the local tracking refresh; reuse upstream route-table APIs and route/interface notifications |
| 06 | Common CIDR Automatic system route planner | `local/route-exclusions` | `f7ad1f1d` | `local-only` | — | Deterministic normalization, subtraction, capacity checks, fuzz coverage, and large fixture |
| 07 | Linux Excluded route prefixes | `local/route-exclusions` | `f7ad1f1d` | `local-only` | — | Common plan installation, incremental ownership, and reverse rollback |
| 08 | macOS Excluded route prefixes | `local/route-exclusions` | `f7ad1f1d` | `local-only` | — | Exclusions after protected-default expansion and owned-route rollback |
| 09 | Windows Excluded route prefixes | `local/route-exclusions` | `f7ad1f1d` | `local-only` | — | Wintun routes derived from the common route plan |
| 10 | Echo prober seam with Local Echo compatibility | `local/direct-echo` | `1fa18904` | `local-only` | — | One lifecycle-aware prober seam for IPv4 and IPv6 with Local Echo as the default |
| 11 | Bounded Direct Echo engine | `local/direct-echo` | `1fa18904` | `local-only` | — | Shared sockets, remapping, matching, limits, carrier replacement, single-stack family degradation, cancellation, and shutdown |
| 12 | Linux Direct Echo transport | `local/direct-echo` | `1fa18904` | `local-only` | — | Raw ICMPv4/ICMPv6 transport bound to the Linux carrier interface |
| 13 | macOS Direct Echo transport | `local/direct-echo` | `1fa18904` | `local-only` | — | Family-specific Darwin raw-socket binding and reply metadata |
| 14 | Direct Echo activation | `local/direct-echo` | `1fa18904` | `local-only` | — | Public configuration, supported-platform activation, startup validation with per-family carrier degradation, ICMP network-unreachable reporting, documentation, and explicit unsupported-platform failure |
| 15 | FreeBSD carrier policy integration | `local/tun-carrier-policy` | `6b4767c5` | `partial` | `c1958dba` | Retain upstream escape FIB implementation; connect it to the lifecycle owner, publish per-family carriers only after successful FIB synchronization, invalidate carriers on synchronization failure, and wait for monitor shutdown using a pollable descriptor |
| 16 | FreeBSD Excluded route prefixes | `local/route-exclusions` | `f7ad1f1d` | `local-only` | — | Apply the common planner after upstream protected-default expansion, validate before interface creation, and propagate route rollback failures |

## 2026-10-11 integration decisions

`main` advanced from `836a6fed` to `701af607` with 6 commits. No file touched by upstream is touched by the local stack, and no behavior unit was absorbed or dropped. The old integration tip was `e8ef14da`; it remains at `backup/dev-before-upstream-sync-20261011-082431`, and the old topic tips at `backup/local-*-before-sync-20261011-082431`.

- `c7dbfd5e` (Hysteria) removes cached clients of stopped instances and locks `Instance.IsRunning`. The QUIC dial path is unchanged and still reaches the required controllers, so unit 02 stays `local-only`.
- `2cc08769` (ECH DoH) only adds MITM context values before the existing `internet.DialSystem` call; unit 02 is unchanged.
- `5af2e9c2` (built-in TLS root CAs), `aa7aa9ad` (MASQUE WARP key handling), `8855145e` (Lua `script` for DNS and routing; see balancer-panel.md) and `701af607` (version) do not touch units 01–16.
- `git range-diff` shows all 15 local commits patch-identical; the rebase had no conflicts.

## Verification at current baseline

Validated on 2026-10-11 against `main@701af607` using Go 1.27.2 on macOS arm64.

- Each of the seven topic commits passed `go build ./...` and `go vet` for `transport/internet`, `features/dns/localdns`, and `proxy/tun/...` on Darwin, plus Windows and Linux vet for `proxy/tun/...`.
- The integration tree passed `go test` and `go test -race` for `./transport/internet ./features/dns/localdns ./proxy/tun/...` and `./infra/conf -run '^TestTunConfig'`, plus `go test` for `transport/internet/hysteria` and `core`, which upstream touched.
- Darwin arm64, Linux arm64/amd64, Windows amd64, and FreeBSD amd64 application builds passed. Windows, Linux, FreeBSD, and Android affected-package test binaries compiled/linked with `-exec=/usr/bin/true`; iOS arm64 `proxy/tun` test binaries compiled/linked with `CGO_ENABLED=1`.
- `app/dns` `TestDOHNameServer` (external DoH EOF) and `TestDNSScriptGeoIPFallback` fail identically on untouched `main@701af607`. Test binaries for `transport/internet/tls`, `masque`, and Hysteria `bbr` could not be built because `github.com/stretchr/testify@v1.12.1` could not be downloaded; those packages built through `go build ./...`.
- Linux privileged smoke tests, Windows/FreeBSD runtime behavior, Android/iOS execution, and privileged macOS TUN end-to-end behavior were not run in this sync.

## 2026-10-10 integration decisions

`main` advanced from `7da5dae6` to `836a6fed` with 7 commits. No complete topic or behavior unit was absorbed or dropped. The old integration tip was `b1ff120c`; it remains at `backup/dev-before-upstream-sync-20261010-090750`, and the old topic tips at `backup/local-*-before-sync-20261010-090750`. No remote refs were changed.

- `8989adfd` adds a Windows `outboundGuard` that turns weak host send off on the `autoOutboundsInterface` interface for the IP versions routed to the TUN, restores it on stop or interface change, and warns while forwarding is on. Windows ignores the carrier binding otherwise, so unit 05 needs this behavior as well. Upstream drives the guard from the legacy `updater.Get()` and its own route/interface callbacks, which unit 05 replaced. Keep the guard's behavior, but read each guarded IP version's carrier from `outboundCarrierPolicy.carrier`, since IPv4 and IPv6 may use different interfaces, and keep state per IP version. `Start` sets the routed IP versions and runs the first check; the local tracking refresh runs it after every route/interface notification; `Close` restores weak host send after tracking stops. No second callback pair or updater is reinstated.
- `1a60fc78` duplicates the iOS NetworkExtension descriptor and always closes Xray's own copy. It merged cleanly into the local Darwin tracking changes; unit 04 is unchanged.
- `9e55a6ed` (MASQUE WARP) changes only TLS, QUIC, and HTTP/3 setup on the existing MASQUE dial paths; its new UDP listeners are loopback test fixtures. No dial path skips the required controllers, so unit 02 stays `local-only`.
- `37184948` (XDNS finalmask), `2eedef51` (DNS pubsub), `2a14c507` (SOCKS UDP statistics) and `836a6fed` (mux UDP) do not touch units 01–16.
- `git range-diff` shows only the carrier-policy commit changed; the other 13 commits are patch-identical.

## Verification at 2026-10-10 baseline

Validated on 2026-10-10 against `main@836a6fed` using Go 1.27.2 on macOS arm64.

- Each of the seven topic commits passed `go build ./...` and `go vet` for `transport/internet`, `features/dns/localdns`, and `proxy/tun/...` on Darwin, plus Windows and Linux vet for `proxy/tun/...`.
- The integration tree passed `go test` and `go test -race` for `./transport/internet ./features/dns/localdns ./proxy/tun/...` and `./infra/conf -run '^TestTunConfig'`, the MASQUE, XDNS, SOCKS and mux package tests, and `go vet -unreachable=false ./infra/conf`.
- Darwin arm64, Linux arm64/amd64, Windows amd64, and FreeBSD amd64 application builds passed. Windows, Linux, FreeBSD, and Android affected-package test binaries compiled/linked with `-exec=/usr/bin/true`; iOS arm64 `proxy/tun` test binaries compiled/linked with `CGO_ENABLED=1`.
- The Windows weak host send guard was compiled and vetted only; it has no unit test and was not run on Windows. Linux privileged smoke tests, Windows/FreeBSD runtime behavior, Android/iOS execution, and privileged macOS TUN end-to-end behavior were not run in this sync.

## 2026-10-07 integration decisions

`main` advanced from `6243d2a2` to `7da5dae6` with 14 commits. No complete topic or behavior unit was absorbed or dropped. The old integration tip was `d8add1e1`; it remains at `backup/dev-before-upstream-sync-20261007-135031`. No remote refs were changed.

- `1f304916` adds Windows WFP leak blocking and DNS skip handling, renames Linux `autoSystemDNS` to `autoSystemDnsToGateway`, and makes system DNS configuration failure stop startup. WFP permits Xray's own traffic, so it does not replace the carrier policy. Callback-registration failure cleanup has small behavioral overlap with unit 05; retain the single local tracking lifecycle rather than reinstating upstream's legacy updater/callbacks.
- Preserve upstream DNS skip checks alongside lifecycle-scoped required controllers in localdns. Windows' new `resolveOnOwn` must also obtain `DialerControllerControl(ctx)` for every DNS dial, rather than iterate the legacy controller list. Its regression test verifies required errors propagate and closing the registration allows later DNS dials.
- System DNS configuration failure now uses the existing startup cleanup function, stopping carrier tracking and releasing policy ownership as well as closing the stack/TUN. This fix belongs to the carrier-policy topic.
- Resolve the protobuf collision by retaining upstream system DNS at **9**, upstream WFP at **10**, local Direct Echo at **11**, and moving local excluded routes to **12**. JSON names for local features are unchanged. This follows the previously confirmed JSON-only configuration contract; old binary protobuf configurations containing field 10 exclusions must be regenerated from JSON or converted with the old schema. A wire-format regression test distinguishes WFP and excluded routes; the DNS/Echo test uses the new DNS JSON name and the required Linux gateway.
- Regenerate `proxy/tun/config.pb.go` with installed `protoc` 36.2 and `protoc-gen-go` v1.36.11.
- Preserve `1e08d48d`'s IPv6-disabled Windows startup handling and `08775afd`'s removal of the UDP packet clone. `e0bae212` adds WireGuard fake pong only; it does not replace TUN Direct Echo.
- The remaining upstream commits do not supply behavior units 01–16. Existing Windows and FreeBSD partial-overlap statuses remain; no additional local feature was removed.

## Verification at 2026-10-07 baseline

Validated on 2026-10-07 against `main@7da5dae6` using Go 1.27.1 on macOS arm64.

- Each of the four topic tips passed `go build ./...`, targeted internet/localdns/TUN tests, `infra/conf` tests matching `^TestTunConfig`, affected-package vet, and Windows TUN/config test compilation. The known unreachable-code analyzer was disabled only for `infra/conf`.
- The integration tree passed `go test -race ./transport/internet ./features/dns/localdns ./proxy/tun/...` and `go test -race ./infra/conf -run '^TestTunConfig'`.
- Darwin, Linux arm64, Windows amd64, and FreeBSD amd64 application builds passed. Linux, Windows, and FreeBSD affected-package test binaries compiled/linked with `-exec=/usr/bin/true`; Windows/FreeBSD were not executed.
- Linux internet, localdns, TUN, and targeted config test binaries ran as root through `orb`. Four `TestLinux.*Smoke` tests passed in temporary, isolated IPv4/IPv6 veth-connected network namespaces: raw sockets, real Direct Echo round trips, excluded-route installation, and carrier tracking startup. The namespaces were removed after the tests.
- Built Darwin and Linux applications passed `run -test` with a combined TUN, route exclusion, and Direct Echo JSON configuration.
- Android arm64 affected-package test binaries compiled except `infra/conf`. The complete Android application failed to link with `github.com/wlynxg/anet: invalid reference to net.zoneCache`; the same error was reproduced on an untouched `main@7da5dae6` worktree.
- iOS arm64 affected-package and targeted config test binaries compiled/linked with `CGO_ENABLED=1`; the linker assumed macOS for Go objects. This does not establish a deployable iOS build.
- Full `go vet ./infra/conf` still reports `infra/conf/xray.go:620:3: unreachable code`, reproduced on untouched `main@7da5dae6`. Other analyzers passed with `-unreachable=false`.
- Reviewed the four-commit `git range-diff`, final residual diff, protobuf generation, and `git diff --check`. Reassigning the DNS failure cleanup to its owning carrier commit left the validated final code tree unchanged.
- Windows/FreeBSD runtime behavior, Android/iOS execution, Linux race tests, privileged macOS TUN end-to-end behavior, and live Linux system DNS takeover remain unverified. The full two-namespace application traffic/carrier replacement scenario from 2026-09-29 was not repeated; the Linux checks above exercise the component smoke tests.


## 2026-09-30 integration decisions

`main` advanced from `e5e85ca9` to `6243d2a2` with six commits. None overlaps units 01–16; every status above is unchanged and no local commit was absorbed or dropped.

- `35e616d3` moves `PacketConnWrapper` from `transport/internet` to `common/net`. It is a type relocation only and does not touch controller registration, so unit 01 stays `local-only`. The one local reference (`connectionRawConn` in `system_dialer_controller_order_test.go`) now uses `net.PacketConnWrapper`; the fix is folded into the unit 01 commit rather than added as a separate commit.
- `fc8f8a45` (XDNS finalmask), `6243d2a2` (geodata matchers), `0fc37920` (HTTPUpgrade headers), `48ad0300` (`api adu` Hysteria) and `08cb6e6b` (WireGuard startup races) do not touch `transport/internet` controllers, `features/dns/localdns`, `proxy/tun`, or `infra/conf/tun.go`.
- `fc8f8a45` also changes `transport/internet/memory_settings.go` so `MemoryStreamConfig.FinalMask` is nil when no TCP/UDP masks are configured (previously always non-nil). This touches the dial path unit 02 depends on, so it was checked by reading code: the nil-mask branches in `proxy/wireguard`, `splithttp`, `hysteria`, `masque` and `kcp` dial through `internet.DialSystem`, which reaches `DefaultSystemDialer.Dial` where required controllers run. Masked paths still reach the same dialer through the `dialTCP`/`dialUDP` closures. No path skips the required controllers, so unit 02 stays `local-only`. No test exercises this end to end.
- `git range-diff` against the previous stack shows only the unit 01 commit changed (the test fix above); the other six commits are patch-identical.

## 2026-09-29 integration decisions

- `efc9e6da` returns an error when the TUN updater has no interface, but ordinary TCP/UDP dialers still log and ignore legacy controller errors. It does not absorb unit 02's fail-closed contract. Required controllers remain necessary.
- Keep `540b9070`'s destination-family UDP wildcard selection alongside the required-controller ordering. Preserve the transport changes from `8267cf95`.
- Keep Windows adapter reuse, startup retries, and route/address/DNS cleanup from `c7245c03` and `5dda894e`. Keep the local combined-metric policy rather than upstream Wi-Fi preference.
- Keep Darwin kqueue waiting from `aa3d6589`, UDP statistics handling from `c412e77a`, and UDP connection cleanup from `7b8ade3e`.
- Keep Linux `autoSystemDNS` from `47a2c2ff`. System DNS takeover and local DNS controller integration are separate features.
- Resolve the protobuf collision by retaining upstream `auto_system_dns = 9`, retaining `auto_system_routing_table_exclude = 10`, and moving `enable_icmp_echo_forwarding` to **11**. JSON names and behavior are unchanged. The operator confirmed JSON-only configuration. Old binary protobuf configurations must be regenerated from JSON or explicitly converted with the old schema; field 9 now means system DNS, not Direct Echo.
- Regenerate only `proxy/tun/config.pb.go` with installed `protoc` 36.2 (`v7.36.2` in the generated header) and `protoc-gen-go` v1.36.11. Keep the truthful generator version in the header.
- FreeBSD now retains upstream automatic routing/FIB support instead of the old local unsupported-platform rejection. Direct Echo remains unsupported there. FreeBSD network behavior has not been run on a FreeBSD host.

## Verification at 2026-09-30 baseline

Validated on 2026-09-30 against `main@6243d2a2` using Go 1.27.1 on macOS arm64. Linux arm64 test binaries and the application were cross-compiled locally and executed through `orb`; the VM has no Go toolchain. Each of the four topic commits was also built (`go build ./...`) and vetted individually, so every layer of the stack compiles on its own.

### Passed

```text
go test ./transport/internet ./features/dns/localdns ./proxy/tun/...
go test ./infra/conf -run '^TestTunConfig'
go test -race ./transport/internet ./features/dns/localdns ./proxy/tun/...
go test -race ./infra/conf -run '^TestTunConfig'
go vet ./transport/internet ./features/dns/localdns ./proxy/tun/...
go vet -unreachable=false ./infra/conf
go build -o <external-output>/xray-darwin ./main
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o <external-output>/xray-linux ./main
```

- macOS tests include real IPv6 UDP socket binding and kqueue lifecycle tests. The built application also passed `run -test` with the combined TUN, exclusion, and Direct Echo JSON configuration.
- Linux executed the `transport/internet`, `features/dns/localdns`, and `proxy/tun` test binaries as root from their package directories (`proxy/tun` reads `testdata/` by relative path), plus `infra/conf` tests matching `^TestTunConfig`.
- Linux executed all four `TestLinux.*Smoke` tests as root in an isolated network namespace with a veth-connected IPv4/IPv6 gateway: raw socket creation, IPv4/IPv6 Direct Echo round trips, automatic route installation, and carrier tracking startup.
- The live two-namespace application test from 2026-09-29 was not repeated. `main` changed nothing under `proxy/tun`, `features/dns/localdns`, or controller registration, and the topic commits are patch-identical to the 2026-09-29 stack except for one test-file line. `main` did change `PacketConnWrapper`'s package and how `FinalMask` is constructed on the dial path (see the integration decisions), so the 2026-09-29 result (TUN-routed Echo in both families, exclusion traffic staying on the physical route, carrier replacement without restart, IPv4 continuing after IPv6 default-route removal, IPv6 network-unreachable reply, TUN removal on shutdown) is carried over from the earlier stack and has not been re-observed on this base.
- Windows amd64 and FreeBSD amd64 test binaries compiled for `transport/internet`, `features/dns/localdns`, `proxy/tun/...`, and `infra/conf`, using `go test -exec=/usr/bin/true` (compile/link only, not execution).
- Android arm64 compiled the same packages except `infra/conf` (see below). iOS arm64 compiled/linked these packages with `CGO_ENABLED=1`; the host linker warned that it assumed macOS for Go object files, so this does not establish a deployable iOS build.
- The protobuf/JSON regression test verifies that system DNS and Direct Echo can be enabled independently or together.
- Reviewed `git range-diff` against the old four-commit stack and checked the final diff for whitespace errors and loss of upstream fixes.

### Known baseline failures and unverified behavior

- Full `go vet ./infra/conf` fails at `infra/conf/xray.go:620:3: unreachable code`. Reproduced on an untouched `main@6243d2a2` worktree. Other vet checks pass with only that analyzer disabled; no unrelated source fix was made.
- Android `infra/conf` test linking fails with `github.com/wlynxg/anet: invalid reference to net.zoneCache`. Reproduced on untouched `main`; not introduced by these patches.
- Windows and FreeBSD runtime network behavior is unverified. Android and iOS were not executed.
- macOS privileged TUN/raw-ICMP end-to-end tests were not run: non-interactive sudo is unavailable. Socket-binding, kqueue, unit, race, build, and configuration checks passed without root.
- Linux race tests were not run: the VM lacks Go and the macOS host lacks a Linux cgo cross-toolchain. macOS race tests passed.
- Linux `systemd-resolved` is inactive, so live system DNS takeover was not exercised. Its existing routing/rollback unit tests passed in the Linux TUN test binary.
- The complete `infra/conf` test suite requires repository geodata assets; only the targeted TUN config suite was executed.

## Earlier synchronizations

The 2026-07-28 and 2026-07-29 syncs were accepted and their `backup/dev-before-upstream-sync-*` refs removed. Earlier pre-reorganization history is no longer preserved: the code tree at the former `local/direct-echo` tip was identical to that old history's tip; only the local commit boundaries changed.

The 2026-07-28 sync reviewed `5b1b4105` and `4aba687d`. `5b1b4105` was patch-equivalent to the previous baseline's `d291486c` process-lookup fix, and `4aba687d` changed gRPC/XHTTP local-address reporting. Neither overlapped units 01–14.

The 2026-07-29 sync reviewed `6ab123bf`, `18e28390`, and `5ca6f4b7`. These added XMC finalmask padding, reduced the XHTTP default `maxConnections`, and bumped the release version. None overlapped units 01–14; the patch queue was rebased verbatim. Targeted tests, race tests, and Linux/Darwin/Windows/FreeBSD/Android/iOS compile checks passed at that baseline.

On 2026-07-30 the queue was refined in place: unit 03 gained refresh failure-log de-duplication, and units 11/14 gained per-family startup degradation and ICMP network-unreachable reporting. Fixups were squashed into their owning patches. Verification included `go test ./proxy/tun/...` and a live IPv4-only host check.

## Synchronization procedure

Use the project skill `/skill:upstream-sync` in `.pi/skills/upstream-sync/SKILL.md`. In this clone, pass `main` explicitly to its preflight helper. Every successful synchronization must update this ledger's checked commit, overlap statuses, upstream references, residual deltas, and verification evidence. Do not treat compile-only checks as platform runtime validation.
