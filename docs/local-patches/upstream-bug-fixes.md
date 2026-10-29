# Local patch ledger: upstream bug fixes

This file tracks fixes for upstream bugs that upstream has not fixed yet. It uses the status vocabulary of `tun-network-path-safety.md`. Drop a fix once upstream fixes the same bug and its test passes without the local change.

## Patch stack

Checked against: `main@2dfee4b8424d814f39407de2c95969a72df519c7` (equal to `upstream/main` at sync time).

Each fix is an independent branch on `main`; `dev` merges them together with `local/direct-echo` and `local/panel`.

## Behavior units

| Issue | Behavior unit | Local branch | Local commit | Upstream status | Residual delta |
|---|---|---|---|---|---|
| F1 | XHTTP/3 failed dial releases its QUIC transport | `fix/xhttp-h3-dial-leak` | `b46b2c94` | `local-only` | `transport/internet/splithttp/dialer.go` closes the `quic.Transport` and its UDP socket when `DialEarly` fails. Upstream returns without closing them, so every failed handshake leaks a socket and the transport's `listen` and `runSendQueue` goroutines. The XDRIVE, MASQUE, and Hysteria dialers already clean up on this path |
| F2 | Freedom UDP socket never shares the port of its TUN source | `fix/tun-udp-self-loop` | `40b0a0e1` | `local-only` | `proxy/freedom/freedom.go` closes and redials a UDP socket whose local port equals the source port of the TUN flow it serves. The flow's source is an address of this host, so otherwise the replies written back to the TUN reach the unconnected socket itself and are forwarded again without end, which pins the CPU and keeps the flow from going idle |

## Background

Both bugs were found on 2026-10-29 on macOS, in an `xray` built from `dev` that had run for about 27 hours:

- F2: the process used about 150% CPU. A capture on the TUN showed only `17.248.216.28.443 > 192.18.0.2.57451` at about 180,000 packets per second, while the physical interface carried no packet for port 57451. The freedom socket was bound to `*:57451` and to the outbound interface with `IPV6_BOUND_IF`; Darwin still delivered the TUN packets to it.
- F1: the goroutine profile held 679 `quic-go.(*Transport).listen` and 679 `runSendQueue` goroutines, and the process held 691 UDP sockets, 673 of which had never received a byte. The only QUIC outbound in the configuration was XHTTP with `alpn: ["h3"]`.

F2 is not limited to Darwin. On Linux, the TUN's `autoOutboundsInterface` binds with `SO_BINDTODEVICE`, and the kernel does not deliver a packet from the TUN to a socket bound to another device, so that setup is not affected. It is affected when outbound sockets leave the TUN through `sockopt.mark` and policy routing or other routing rules, since those do not filter receiving. Windows (`IP_UNICAST_IF`) and FreeBSD (`SO_SETFIB`) are expected to be affected by the same reasoning; only Darwin was observed.

Upstream overlap signals to watch: changes to the `DialEarly` error path in `transport/internet/splithttp/dialer.go` (F1), and any upstream handling of the freedom socket port or of TUN replies addressed to Xray's own sockets (F2). On 2026-10-29 a search of XTLS/Xray-core issues and pull requests found neither bug; related but different issues are #7160 (an unbound dial looping into the TUN), #6328 (H3 ports kept on XMUX rotation, fixed), and #5956 (splithttp reconnect leaks, not merged). apernet/quic-go has issues disabled and no related pull request; the transport's lifetime belongs to its caller.

## Verification at current baseline

Validated on 2026-10-29 against `main@2dfee4b8` using Go 1.27.3 on macOS arm64.

- `TestH3DialFailureReleasesTransport` (F1) dials HTTP/3 three times to a UDP endpoint that never answers and requires no `quic-go.(*Transport).listen` goroutine to remain. It fails with "3 QUIC transports still listening" without the fix.
- `TestUDPRedialsWhenSocketTakesTunSourcePort` (F2) drives `freedom.Handler.Process` with a dialer whose first socket holds the TUN source port and requires that socket to be closed and a second one dialed. It fails without the fix.
- `go test` and `go test -race` passed for `./proxy/freedom` and `./transport/internet/splithttp` on each fix branch and on the integration tree, together with the TUN and panel checks recorded in the other ledgers.
- The fixes were not run in the live process; the operator rebuilds and restarts it.
