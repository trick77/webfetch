# AGENTS.md

## What this is

`webfetch` (`github.com/trick77/webfetch`) is a dependency-light Go **library** —
a single package at the repo root, no CLI and no server binary. It ports the
Python `mcp-server-fetch`. Callers use it in-process:
`webfetch.Fetch(ctx, url, webfetch.Options{...})`.

Staying fully **in-process** — no subprocess, no sidecar container — is the whole
point of the port. Keep it that way: any new dependency must be pure-Go (the PDF
path uses a pure-Go parser for exactly this reason), and keep the dependency set
minimal.

## Commands

Run all of these before declaring a change done (they mirror CI, in order):

```
gofmt -l .          # must print nothing
go vet ./...
go build ./...
go test -race ./...
```

Go 1.25. There is no Makefile and no golangci-lint — `gofmt` + `go vet` are the
only style gates.

## Hard constraint — upstream fidelity

This library deliberately reproduces the *observable contract* of upstream
`mcp-server-fetch`. Do NOT rephrase, reformat, or "improve":

- the exact upstream strings: the `<error>…</error>` messages, the
  `Contents of %s:` prefix, and the "Content type … cannot be simplified" note
- the autonomous `User-Agent` (`DefaultUserAgentAutonomous`)
- the html-to-markdown config (`atx` headings, `*` bullets, `*` emphasis) — this
  matches Python markdownify defaults; changing it breaks byte-parity
- rune-based (not byte-based) `start_index` / `max_length` slicing, which matches
  Python `str` slicing

**Extending is fine; diverging by default is not.** Capabilities beyond upstream
are added as opt-in `Options` fields that default to off, so a zero-value
`Options` produces byte-identical output. Follow that pattern for anything new.
Existing extensions: `IncludeMetadata`, `ExtractPDF`, and the `FullPage` /
`Selector` / `ExcludeSelectors` escape hatch.

## Hard constraint — SSRF guard (`ssrf.go`)

Strict default-deny: only globally-routable public unicast IPs may be reached.
Enforcement lives in `net.Dialer.Control`, which runs *after DNS resolution* so
it also covers redirects and DNS-rebinding. Do not weaken it, do not move the
check to the hostname level, and do not add exceptions. Apply any change
consistently across IPv4 and IPv6, including IPv4-mapped IPv6 forms. `dialControl`
is a test-only hook so in-package tests can reach a loopback server — don't
repurpose or export it.

## Git & CI

- Default branch is `master`. Never push to `master` — always branch + PR.
- CI tests run on branches/PRs only, never on the master push.
- Merging to `master` (touching `**.go` / `go.mod` / `go.sum`) auto-mints a
  semver tag via the release workflow — a merge is a release.
- Dependabot opens one grouped dependency PR per week; merge it manually after CI passes.
- All workflow and config files use the `.yaml` extension (never `.yml`).
