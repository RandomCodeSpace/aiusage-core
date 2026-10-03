# Source and extraction

The initial library was extracted from
[`RandomCodeSpace/aiusage` at `c96f6cbad1a30405fa3789cbca0f91aa4d66b396`](https://github.com/RandomCodeSpace/aiusage/tree/c96f6cbad1a30405fa3789cbca0f91aa4d66b396)
on 2026-10-03.

Copied package trees are `model`, `adapter`, `collect`, `store`, and `pricing`,
including their tests, fixtures, embedded SQLite schema, price snapshots, and
Models.dev license notice. The root MIT license is unchanged. Local Go imports
were changed from `github.com/RandomCodeSpace/aiusage` to
`github.com/RandomCodeSpace/aiusage-core`; runtime logic and public API shapes
were preserved. Go types now belong to the new module path, so consumers must
use core types consistently rather than mixing both modules' model packages.

Application commands, TUI, dashboard, configuration loader, daemon supervision,
and binary release workflows were not copied. The build-excluded
`pricing/gensnapshot.go` maintenance tool stays with the snapshots it generates.
New files provide embedding documentation, a package example, and library CI.

The module retains the original Go 1.25.13 minimum and dependency versions.
Its direct dependencies are `github.com/klauspost/compress v1.19.2` for DSH
zstd logs and `modernc.org/sqlite v1.50.1` for source databases and the ledger.
Their imported code uses BSD-3-Clause licenses compatible with this MIT
library. Both projects have maintained upstream releases. An OSV query for
each exact module version returned no advisories on 2026-10-03. This check
covered those two direct dependencies, not a full transitive audit.

Sources for the dependency check are the
[compress release](https://github.com/klauspost/compress/releases/tag/v1.19.2),
[SQLite project](https://github.com/modernc-org/sqlite), their versioned module
license files, and the [OSV API](https://api.osv.dev/v1/query).
