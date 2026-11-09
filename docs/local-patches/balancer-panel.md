# Local patch ledger: balancer panel

This file tracks the local web panel for inspecting and switching balancers. It uses the status vocabulary of `tun-network-path-safety.md`.

## Patch stack

Checked against: `main@676eaf66f8f05d4d558ee7a74b4736623d3feeb9` (equal to `upstream/main` at sync time).

`local/panel` is based directly on `main` and does not depend on any TUN behavior; `dev` merges it with `local/direct-echo` and the `fix/*` branches.

## Configuration contract

The panel has no configuration of its own. When `metrics.listen` is set, the metrics HTTP server serves it under `/panel/`:

```json
"metrics": { "listen": "127.0.0.1:10121" }
```

The panel is disabled with a warning when the instance does not use the built-in `app/router`. `/debug/vars` and pprof share the listener, so bind it to loopback only. Requests with a non-loopback `Host` header are rejected, and POST bodies must be `application/json`.

## Behavior units

| Issue | Behavior unit | Local commit | Upstream status | Residual delta |
|---|---|---|---|---|
| P1 | Router inspection | `126a6e95` | `local-only` | `Router.ListBalancers` (selectors, strategy, fallback, candidates, override, principle) and `Router.ListRuleTargets` (rule to outbound or balancer tag); upstream `ListRule` omits balancer tags and there is no balancer listing |
| P2 | Atomic rule balancer switch | `126a6e95` | `local-only` | `Router.SetRuleBalancer` swaps the rule slice under the router mutex; upstream only offers RemoveRule then AddRule, which leaves a window where traffic misses the rule |
| P3 | Latest burst probe result | `e2ce7078` | `local-only` | `burst.Observer.LatestResults`; upstream exposes only window statistics, so a single on-demand `Check` result is not observable |
| P4 | Panel HTTP handler | `b41bf615` | `local-only` | `app/panel` (embedded page and JSON API: status, override, rule switch, route test, on-demand check) and one mount in `app/metrics.httpHandler` |
| P5 | Global routing mode | `5760d624` | `local-only` | `Router.SetRoutingMode`/`GetRoutingMode` (`config`, `proxy` → balancer tag, `direct` → outbound tag), held in memory only. In `proxy` and `direct` mode `PickRoute` applies only rules whose outbound is a `dns` proxy; every other rule is skipped without being applied, so it triggers no DNS lookup and the IpIfNonMatch second pass does not run. Unmatched traffic goes to the mode target with an empty rule tag; the balancer is looked up by tag on each pick. `RuleTarget.KeptInGlobalMode` reports the kept rules |
| P6 | Panel routing mode switch | `121c4ecc` | `local-only` | `POST api/mode` (`direct` accepts only freedom outbounds); `api/status` gains `mode`, `directOutbounds`, and per-rule `keptInGlobal`; the page gains a proxy/config/direct switch and dims skipped rules |

Interaction with upstream Lua routing (`8855145e`, checked 2026-10-11): when `routing.script` is set, `Router.PickRoute` returns the script's result and never consults the rule list. P1 still lists rules and balancers, and P2 still swaps the rule slice, but neither changes routing decisions in that mode; the panel's route test follows the script because it calls `PickRoute`. The script's `xray.router.PickOutbound` calls `Balancer.PickOutbound`, so panel overrides still apply to balancers picked from Lua. Upstream supplies none of P1–P4, so they stay `local-only`; no code change was made. P5 rejects `SetRoutingMode` for `proxy` and `direct` when a script is set, because the mode would have no effect.

Global mode also skips any rule that forwards API (commander) or reverse traffic; the panel itself is served by the metrics listener and does not pass through the router. A rule kept in global mode is identified by the target handler's proxy type (`*proxy/dns.Handler`), so a change to how `outbound.Handler` exposes its proxy breaks P5; `TestPanelRoutingMode` checks this with real handlers.

Upstream overlap signals to watch: new RoutingService methods for balancer listing or rule updates (P1, P2), an ObservatoryService method for on-demand checks or per-sample results (P3), changes to `app/metrics.httpHandler` (P4 mount conflict), and a global, override, or catch-all mode in `Router.PickRoute` or RoutingService (P5).

## Verification at current baseline

Rebased on 2026-11-09 onto `main@676eaf66` without conflicts; `git range-diff` shows all five commits (`126a6e95`, `e2ce7078`, `b41bf615`, `5760d624`, `121c4ecc`) patch-identical. The rebuilt `dev` merges `local/panel` once, in the single `--no-ff` merge, so the separate `fd8f8015` merge is gone. P1–P6 stay `local-only` with the residual deltas above; upstream commits implementing them: none. The 24 upstream commits do not touch `app/router`, `app/metrics`, `app/observatory`, or `app/panel`. `2e4c13d8` changes only how the inbound manager handles a handler whose `Start` fails; the panel looks up `inbound.Manager` and does not add inbounds. `63ca2e0c` renames the freedom `finalRules` field `blockDelay` to `delay` and adds `logLevel`; P6 only checks that a `direct` target is a freedom outbound, so it is unaffected.

Each panel commit passed `go build ./...`. On the topic tip, `go test ./app/panel ./app/metrics ./app/observatory/burst` and `go test ./app/router -skip TestChinaSites` passed. On the integration, the same tests passed, and race tests passed for panel/router tests matching `Panel|ListBalancers|SetRuleBalancer|RoutingMode|RuleTargets` and burst tests matching `Latest`. Darwin vet passed for `app/panel`, `app/router`, `app/metrics`, and `app/observatory/burst`. Test binaries for panel, metrics, router, and burst compiled/linked for Darwin arm64, Linux arm64/amd64, Windows amd64, and FreeBSD amd64 with `-exec=/usr/bin/true`; foreign binaries were not executed. All of this used Go 1.27.3 on macOS arm64. No panel smoke test against a running instance was repeated.

## Verification at `main@a4a3dff5`

Added P5–P6 on 2026-11-09 on top of `7e6be209` at `main@a4a3dff5`, and merged `local/panel` into `dev` as `fd8f8015`. `dev` now has a second topic merge after the earlier documentation commits; the next rebuild folds it into the single `--no-ff` merge. The P5 commit on its own built and passed `go vet` plus the router, panel, metrics, and burst tests. On the merged `dev` tree, everything in the upstream-sync checklist passed, plus `go vet ./app/router ./app/panel`, `go build ./main`, and `go test ./app/dispatcher`, using Go 1.27.3 on macOS arm64. The panel/router race regex now also matches `RoutingMode|RuleTargets`. `go vet ./app/router/...` still reports the existing warning at `app/router/command/command.go:101`, an untouched file. On 2026-11-08, a temporary unprivileged instance (socks inbound, freedom nodes, `dns-out`, two balancers) was driven through the page and `api/route`:
- In `direct` and `proxy` mode, port 53 still reached `dns-out` and all other traffic went to the target.
- Switching back to `config` restored the per-rule outbounds.

The live service was not touched.

Rebased on 2026-11-01 onto `main@a4a3dff5` without conflicts; `git range-diff` shows all three commits (`0b932a67`, `e419da9e`, `7e6be209`) patch-identical. P1–P4 retain the residual deltas above. Upstream commits implementing P1–P4: none. Inspected `6ca6d8a6`: the CLI now loops over explicitly supplied balancer tags using the existing `GetBalancerInfo`; it adds neither enumeration/configuration inspection nor atomic rule switching. `d1140de2` only updates protobuf generator headers in these packages. The XHTTP server changes in `316e07a5` and `e7a69fa8` do not alter the metrics listener or panel handler.

Each panel commit passed `go build ./...`. On the topic tip and integration, `go test ./app/panel ./app/metrics ./app/observatory/burst`, `go test ./app/router -skip TestChinaSites`, and affected-package vet passed using Go 1.27.3 on macOS arm64. Integration race tests passed for panel/router tests matching `Panel|ListBalancers|SetRuleBalancer` and burst tests matching `Latest`. The panel package's loopback HTTP end-to-end tests ran; the live outbound probe smoke from 2026-10-09 was not repeated. Test binaries for panel, metrics, router, and burst also compiled/linked for Darwin arm64, Linux arm64/amd64, Windows amd64, and FreeBSD amd64 with `-exec=/usr/bin/true`; foreign binaries were not executed.

## Earlier verification

Rebased on 2026-10-29 onto `main@0afec660` without conflicts; `git range-diff` shows the three commits (now `537bc579`, `88eb59b0`, `2028b710`) patch-identical. The two upstream commits touch only `infra/conf/xray.go`, its test, and `core/core.go`, so P1–P4 stay `local-only`. `55eecaed` creates the inbound manager after the metrics app; `panel.New` looks up `inbound.Manager` only from `MetricsHandler.Start`, after every feature is registered, so the panel is unaffected. Each commit passed `go build ./...`; `go test ./app/panel ./app/metrics ./app/observatory/burst`, `go test ./app/router -skip TestChinaSites`, and the race tests below passed on the integration tree using Go 1.27.3 on macOS arm64. On the `local/panel` tip, one `go test ./app/panel ./app/metrics ./app/observatory/burst` run reported `app/panel` failing; its output was not captured, and 240 later runs of `app/panel`, alone and together with the other two packages, all passed. `startServer` picks the metrics port by listening on `127.0.0.1:0` and closing it before Xray binds it, so another process can take the port in between; this is the suspected cause but was not confirmed. The smoke test was not repeated.

Rebased on 2026-10-29 onto `main@2dfee4b8` without conflicts; `git range-diff` shows the three commits (now `da0933aa`, `4f3f086e`, `1b43460f`) patch-identical. Of the 17 upstream commits, only `78b5a6ec` touches these areas: it reflows comments in `app/router/command/command.proto` and regenerates `app/observatory/config.pb.go`, adding no RoutingService or ObservatoryService method and leaving `app/metrics.httpHandler` unchanged, so P1–P4 stay `local-only`. Each commit passed `go build ./...` and `go vet` for the panel packages that exist at that commit; `go test ./app/panel ./app/metrics ./app/observatory/burst`, `go test ./app/router -skip TestChinaSites`, and the race tests below passed on the new tip and on the integration tree using Go 1.27.3 on macOS arm64. The smoke test was not repeated.

Rebased on 2026-10-22 onto `main@1b1647cd` without conflicts; `git range-diff` shows the three commits (now `c593eaac`, `77c93fe2`, `c9af85f4`) patch-identical. The 13 upstream commits do not touch `app/router`, `app/metrics`, `app/observatory`, or `app/panel`. Each commit passed `go build ./...` and `go vet` for the panel packages that exist at that commit; `go test ./app/panel ./app/metrics ./app/observatory/burst`, `go test ./app/router -skip TestChinaSites`, and the race tests below passed on the new tip using Go 1.27.2 on macOS arm64. The smoke test was not repeated.

Rebased on 2026-10-19 from `local/direct-echo` onto `main@92ccc939` without conflicts; `git range-diff` shows the three commits patch-identical. Each commit passed `go build ./...` and `go vet` for the panel, metrics, router, and burst packages that exist at that commit; the package tests and race tests below passed on the new tip.

Revalidated on 2026-10-19 against `main@92ccc939` using Go 1.27.2 on macOS arm64. Upstream changed `app/router` (`5a64db00` inline Lua scripts, `ac5d8a83` Lua sandboxing; both only touch script loading and the `PickRoute` script branch) and one line of `app/observatory/burst/healthping_result.go` (`d15966c7`, average RTT arithmetic), but no file in this patch. The three panel commits are patch-identical and applied without conflict onto the rewritten TUN stack. `go test ./app/panel ./app/metrics ./app/observatory/burst`, `go test ./app/router -skip TestChinaSites`, the race tests below, and `go build ./...` at each commit passed again. The smoke test was not repeated.

Revalidated on 2026-10-11 against `main@701af607` using Go 1.27.2 on macOS arm64. Upstream changed `app/router/router.go` (Lua script fields and the `PickRoute` branch) but no file in this patch; the three panel commits are patch-identical. `go test ./app/panel ./app/metrics ./app/observatory/burst`, `go test ./app/router -skip TestChinaSites`, the race tests below, and `go vet` for the four packages passed again. The smoke test was not repeated.

Revalidated on 2026-10-10 against `main@836a6fed` using Go 1.27.2 on macOS arm64: the seven upstream commits do not touch `app/router`, `app/metrics`, `app/observatory`, or `app/panel`, the three panel commits are patch-identical, and the package tests and race tests below passed again. The smoke test was not repeated.

Validated on 2026-10-09 against `main@7da5dae6` using Go 1.27.2 on macOS arm64.

- `go test ./app/panel ./app/metrics ./app/observatory/burst` and `go test ./app/router -skip TestChinaSites` passed. `TestChinaSites` needs `resources/geosite.dat` and fails identically on `local/direct-echo`.
- `go test -race` passed for the panel end-to-end tests and the router inspection tests.
- `go vet` passed for `app/panel`, `app/metrics`, `app/router`, and `app/observatory/burst`.
- Smoke test: an application built with the `dev` build command ran unprivileged with the live configuration minus the TUN inbound, outbounds bound to the physical interface, and `metrics.listen` on loopback. The page loaded; status listed 14 rules and 5 balancers; single-node and whole-group checks returned fresh samples (about 1.3 s and 2.7 s) that updated the window statistics; switching `final` changed `PickRoute` results; rule switches to an unknown balancer or an outbound rule and checks of unobserved tags were rejected; override set and clear worked.
- Without interface binding, an unprivileged instance's REALITY handshakes enter the running TUN, whose TLS sniffing redirects them to the real server name. This is a test-environment effect, not a panel behavior.
