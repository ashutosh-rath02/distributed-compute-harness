# Contributing

Home Harness is a Go project (no cgo) that runs one manager plus agents on a
person's own Windows, Linux, macOS and Android devices. Read `HANDOVER.md`
first: §2 (the rule that must not be broken), §11 (gotchas) and §12
(conventions) save the most time.

## Build

```bash
go build ./...                       # everything
make build                           # bin/{manager,agent,relay,harnessctl} + bin/agents/* for every platform
bash scripts/build-agents.sh         # just the agents
bash scripts/check-agents.sh         # the manager's own check of that set
```

Binaries report their version with `-version`. Builds made by the scripts
carry `0.0.0-dev+<commit>`; set `HARNESS_VERSION=1.2.3` to override
(`scripts/version.sh` explains the rest). Self-update compares binary
hashes, never version strings.

## Test

```bash
make lint          # go build, go vet, gofmt check that ignores CRLF
make test          # unit tests: go test -p 1 ./internal/... ./cmd/... ./scripts/...
make integration   # go test -p 1 ./test/integration
```

Without `make` (Windows without it installed), run the commands inside the
Makefile directly.

### Running the suite safely

- **Always `-p 1`.** Unit tests in several packages and every integration
  test bind fixed `127.0.0.1` ports; two test binaries at once collide and
  fail for reasons that have nothing to do with your change.
- **One suite per machine.** Do not run the integration suite while another
  suite, a live manager (ports 7419-7421) or a real-binary run is going.
  New integration tests take ports from a range nobody else uses; check
  `grep -rn "127.0.0.1:19" test/integration` before picking one.
- **Bind 127.0.0.1 only.** A test that listens on `0.0.0.0` triggers the
  Windows Firewall popup, which blocks the run until someone clicks it
  (HANDOVER §11). For real-binary runs, build to a fixed path so the
  firewall rule you approved once keeps matching.
- **Sleeping laptops fail tests.** Modern Standby after 15 idle minutes
  turns a long run into meaningless timeouts; keep the machine awake.
- **`websocket` `Close()` can take about 5 seconds** with no receive loop
  on the other side; slow transport tests are expected, not hung.
- **No `-race` on a machine without a C compiler.** CI runs it on Linux.
  Locally, use `go test -count=3` to hunt flakes.
- **Never weaken an assertion to make a flaky test pass.** Find the race
  (a missing wait for a state, a timing assumption) and fix that.
- On Windows the working copy may be CRLF while git stores LF.
  `.gitattributes` makes new checkouts LF; `scripts/fmt-check.sh` ignores
  line endings either way.

## Conventions

- Plan before building anything non-trivial. Match the style and comment
  density around you: comments explain *why*, user-facing text is plain
  English, and nothing is tied to one device type.
- Small packages, standard library first; a new dependency needs a reason.
- The manager never trusts agents, AI models or relays. Every non-`/v1`
  route needs the operator token (`internal/manager/operatorauth.go`);
  keep the public list (`operatorPublic`) as short as it is. Operator
  routes never change state on GET and are registered in `NewHTTPHandler`.
- The dashboard escapes every agent- or model-supplied string
  (`escapeHtml`/`textContent`). `TestPageScriptsParse` plus `node --check`
  guard its scripts.
- Mixed-version fleets: new agent behaviour goes behind
  `Manifest.AgentFeatures`, and an existing catalog type's version is never
  bumped.
- Write the plan/why in the commit message. Keep deferred scope written
  down, in code comments or the commit, instead of dropping it.

## Adding a task type

1. Define it in `internal/catalog` (name, version, parameter schema with
   bounds, input and output rules, default requirements). One schema serves
   both the manager's validation and the agent's re-check.
2. Implement the handler in `internal/tasks` and register it. It runs
   natively in Go: no shell, no user-supplied flags, outputs declared up
   front. Refuse anything outside the schema again here.
3. Capability advertising: the agent offers the type only when it can run
   it (tool installed, runtime present). Gate any new agent behaviour with
   `AgentFeatures`.
4. Policy: decide if it is allowed by default; risky types start off.
5. Tests: a catalog schema test, a handler unit test (with hostile input),
   and an integration test with a real in-process manager and agent.
6. Add a line to `CHANGELOG.md` and the relevant `HANDOVER.md` section.

## Review checklist

- [ ] `make lint` and the tests pass; new logic has a test that fails when
      the logic is broken (try it).
- [ ] No new unauthenticated route; no state change on GET.
- [ ] Agent, model and relay input is validated and escaped on display.
- [ ] Old agents keep working (feature flag, no version bump of existing types).
- [ ] No secrets in logs, URLs or `GET /version`-style public routes.
- [ ] Ports and files used by tests are loopback-only and don't collide.
- [ ] Docs and `CHANGELOG.md` updated; commit message says why.

## Releases

Tag `vMAJOR.MINOR.PATCH`; `.github/workflows/release.yml` builds, signs and
drafts the release. It needs the repository secret `HARNESS_RELEASE_KEY`
(the PEM text of `~/.home-harness/release-signing.key`; back that file up).
`make release-local` does the same into `dist/release/` with your own key.
