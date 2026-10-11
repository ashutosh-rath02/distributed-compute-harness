# Third-party software

Checked 2026-10-09 with Go 1.27.0. `go mod verify` reports "all modules verified" (every module in the cache matches `go.sum`).

## Direct dependencies

| Module | Version | License | Used for |
|---|---|---|---|
| `go.etcd.io/bbolt` | v1.5.0 | MIT | the manager's embedded database |
| `github.com/shirou/gopsutil/v4` | v4.26.8 | BSD-3-Clause | CPU, memory, disk and uptime readings in the agent |
| `golang.org/x/sys` | v0.48.0 | BSD-3-Clause | OS calls (file locks, power, process control) |
| `nhooyr.io/websocket` | v1.8.17 | ISC | the agent transport and the relay |

## Indirect dependencies (pulled in by gopsutil)

| Module | Version | License |
|---|---|---|
| `github.com/ebitengine/purego` | v0.10.2 | Apache-2.0 |
| `github.com/go-ole/go-ole` | v1.2.6 | MIT |
| `github.com/lufia/plan9stats` | 2021-10-12 | BSD-3-Clause |
| `github.com/power-devops/perfstat` | 2024-02-21 | MIT |
| `github.com/tklauser/go-sysconf` | v0.3.16 | BSD-3-Clause |
| `github.com/tklauser/numcpus` | v0.11.0 | Apache-2.0 |
| `github.com/yusufpapurcu/wmi` | v1.2.4 | MIT |

No GPL, AGPL or other copyleft licence is in the build. Everything is cgo-free.

## Bill of materials

`docs/sbom.cdx.json` is a CycloneDX 1.6 module-level SBOM. Regenerate it with a pinned tool, no install needed:

    go run github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@v1.12.0 mod -json -output docs/sbom.cdx.json .

The tool warns that it cannot read a version for the main module when the repository has no tags; that only affects the root component's version.

## Vulnerabilities and staleness

`go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...` found **no vulnerable third-party module**. It found 13 standard-library advisories (GO-2026-6599 to GO-2026-6617: `net/http`, its HTTP/2 code, `net/textproto`, `crypto/tls`, `html/template`, `os`), all **fixed in Go 1.27.2**, and the build machine has Go 1.27.0. The manager's operator API, join page and agent listener use `net/http` and `crypto/tls`, so **release builds must use Go 1.27.2 or newer**. This is the one item here that should be acted on.

Modules with newer releases (none with a known advisory): gopsutil v4.26.9, x/sys v0.49.0, purego v0.11.1, go-ole v1.3.0, go-sysconf v0.4.0, numcpus v0.12.0, plan9stats and perfstat (years old, unmaintained-looking but tiny and read-only). `go-ole` and `wmi` are Windows-only. `github.com/aclements/go-moremath`, `golang.org/x/perf`, `golang.org/x/tools` and `cobra` are in the module graph only; `go list -deps -test ./...` shows none of them is imported by this repository.

Risk notes: `nhooyr.io/websocket` has moved to `github.com/coder/websocket` upstream; v1.8.17 is the last release under the old path and works, but plan a move. `plan9stats` and `perfstat` have had no release for a long time; they are only reached through gopsutil.
