# Local patch ledger: balancer panel

This file tracks the local web panel for inspecting and switching balancers. It uses the status vocabulary of `tun-network-path-safety.md`.

## Patch stack

Checked against: `main@701af60772cda123492f86e383e1fdba066614a9` (equal to `upstream/main` at sync time).

`local/panel` stacks on `local/direct-echo` but does not depend on any TUN behavior; it can be rebased onto any parent that builds.

## Configuration contract

The panel has no configuration of its own. When `metrics.listen` is set, the metrics HTTP server serves it under `/panel/`:

```json
"metrics": { "listen": "127.0.0.1:10121" }
```

The panel is disabled with a warning when the instance does not use the built-in `app/router`. `/debug/vars` and pprof share the listener, so bind it to loopback only. Requests with a non-loopback `Host` header are rejected, and POST bodies must be `application/json`.

## Behavior units

| Issue | Behavior unit | Local commit | Upstream status | Residual delta |
|---|---|---|---|---|
| P1 | Router inspection | `eaa8b549` | `local-only` | `Router.ListBalancers` (selectors, strategy, fallback, candidates, override, principle) and `Router.ListRuleTargets` (rule to outbound or balancer tag); upstream `ListRule` omits balancer tags and there is no balancer listing |
| P2 | Atomic rule balancer switch | `eaa8b549` | `local-only` | `Router.SetRuleBalancer` swaps the rule slice under the router mutex; upstream only offers RemoveRule then AddRule, which leaves a window where traffic misses the rule |
| P3 | Latest burst probe result | `66c3facc` | `local-only` | `burst.Observer.LatestResults`; upstream exposes only window statistics, so a single on-demand `Check` result is not observable |
| P4 | Panel HTTP handler | `c07c9c6e` | `local-only` | `app/panel` (embedded page and JSON API: status, override, rule switch, route test, on-demand check) and one mount in `app/metrics.httpHandler` |

Interaction with upstream Lua routing (`8855145e`, checked 2026-10-11): when `routing.script` is set, `Router.PickRoute` returns the script's result and never consults the rule list. P1 still lists rules and balancers, and P2 still swaps the rule slice, but neither changes routing decisions in that mode; the panel's route test follows the script because it calls `PickRoute`. The script's `xray.router.PickOutbound` calls `Balancer.PickOutbound`, so panel overrides still apply to balancers picked from Lua. Upstream supplies none of P1–P4, so they stay `local-only`; no code change was made.

Upstream overlap signals to watch: new RoutingService methods for balancer listing or rule updates (P1, P2), an ObservatoryService method for on-demand checks or per-sample results (P3), and changes to `app/metrics.httpHandler` (P4 mount conflict).

## Verification at current baseline

Revalidated on 2026-10-11 against `main@701af607` using Go 1.27.2 on macOS arm64. Upstream changed `app/router/router.go` (Lua script fields and the `PickRoute` branch) but no file in this patch; the three panel commits are patch-identical. `go test ./app/panel ./app/metrics ./app/observatory/burst`, `go test ./app/router -skip TestChinaSites`, the race tests below, and `go vet` for the four packages passed again. The smoke test was not repeated.

Revalidated on 2026-10-10 against `main@836a6fed` using Go 1.27.2 on macOS arm64: the seven upstream commits do not touch `app/router`, `app/metrics`, `app/observatory`, or `app/panel`, the three panel commits are patch-identical, and the package tests and race tests below passed again. The smoke test was not repeated.

Validated on 2026-10-09 against `main@7da5dae6` using Go 1.27.2 on macOS arm64.

- `go test ./app/panel ./app/metrics ./app/observatory/burst` and `go test ./app/router -skip TestChinaSites` passed. `TestChinaSites` needs `resources/geosite.dat` and fails identically on `local/direct-echo`.
- `go test -race` passed for the panel end-to-end tests and the router inspection tests.
- `go vet` passed for `app/panel`, `app/metrics`, `app/router`, and `app/observatory/burst`.
- Smoke test: an application built with the `dev` build command ran unprivileged with the live configuration minus the TUN inbound, outbounds bound to the physical interface, and `metrics.listen` on loopback. The page loaded; status listed 14 rules and 5 balancers; single-node and whole-group checks returned fresh samples (about 1.3 s and 2.7 s) that updated the window statistics; switching `final` changed `PickRoute` results; rule switches to an unknown balancer or an outbound rule and checks of unobserved tags were rejected; override set and clear worked.
- Without interface binding, an unprivileged instance's REALITY handshakes enter the running TUN, whose TLS sniffing redirects them to the real server name. This is a test-environment effect, not a panel behavior.
