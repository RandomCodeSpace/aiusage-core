# Security policy

Report vulnerabilities through the repository's
[private vulnerability reporting form](https://github.com/RandomCodeSpace/aiusage-core/security/advisories/new).
Include the library version or commit, Go version, platform, affected adapter,
and a minimal reproduction with synthetic data. Do not include credentials,
real conversations, or an unsanitized usage database in a report. Use public
issues for ordinary bugs that do not disclose a vulnerability.

Until the first release, security fixes target the latest `main` commit. After
that, fixes target the latest release and users of older releases may need to
upgrade. Reports are assessed against the boundaries below; passing automated
checks is not a guarantee that the library has no vulnerabilities.

## Trust boundaries

This library runs with its embedding process's filesystem permissions. Use
trusted local source roots, ledger paths, backup paths, and pricing-cache
directories. Do not let an untrusted request choose those paths.

Source roots are discovery settings, not filesystem confinement. Adapters can
follow symlinks to regular files, and source indexes can name paths outside a
configured root. A regular-file check does not protect against another process
replacing a file between checking and opening it. Isolate collection in a
restricted process if other users or processes can control the source tree.

Adapters open source databases read-only and do not update source rows.
SQLite may create WAL coordination sidecars. The output ledger, recovery
backups, and optional price caches are writable application data. The library
does not add encryption or authentication to them; the embedding application
owns their access controls and retention.

There is no general per-file or per-cycle memory bound. Some JSON sources and
cached price tables are read as whole files. Compressed sources have decoder
and output limits, but those are not a small-process memory budget. Context
cancellation does not interrupt every underlying filesystem operation. Treat
hostile, oversized, or concurrently replaced source files as requiring process
isolation and resource limits, not as a supported input boundary.

Adapters extract usage from records that can contain conversation text. Public
model types do not provide fields for prompts, command arguments, or message
contents. Audit payloads use allow-lists, and `collect.WithoutRaw()` drops those
payloads before storage. Source paths, project paths, session IDs, and activity
names remain potentially sensitive metadata. `WithoutRaw` does not anonymize
the ledger.

Optional pricing refresh contacts fixed LiteLLM and Models.dev HTTPS URLs. It
uses the standard HTTP client's redirect behavior. It does not query provider
usage APIs or require provider credentials. Leave refresh disabled when the
embedding application must perform no network requests.

## Checks and updates

[CI](.github/workflows/ci.yml) runs the fixture suite, the Go race detector, and
`govulncheck` with a pinned scanner and the live Go vulnerability database. The
scan checks reachable vulnerabilities for the Go version in `.go-version`.
Consumers should also scan their complete application with its actual Go
toolchain and dependency graph. No vulnerability scan covers every possible
behavior or privately known issue.

Go and dependency updates must preserve the
[compatibility and accounting contracts](COMPATIBILITY.md), or document the
required migration. The fixture tests and supported-target checks run again
when those versions change.
