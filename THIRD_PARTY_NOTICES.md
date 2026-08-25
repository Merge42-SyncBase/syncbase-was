# Third-party notices

SyncBase WAS is licensed under Apache-2.0. The following direct runtime
dependencies are not relicensed by SyncBase.

| Component | Version | Use | Upstream license | Source |
| --- | --- | --- | --- | --- |
| `github.com/Merge42-SyncBase/syncbase-embedding` | Pseudo-version pinned in `go.mod` | Local E5 embedding adapter | Apache-2.0, first-party sibling project | <https://github.com/Merge42-SyncBase/syncbase-embedding> |
| `github.com/klippa-app/go-pdfium` | v1.19.6 | PDFium WebAssembly binding and embedded engine | Binding: MIT, copyright 2022 Klippa App BV. Its pinned upstream README declares the embedded PDFium engine and Wazero runtime Apache-2.0. | <https://github.com/klippa-app/go-pdfium/tree/v1.19.6> |
| `github.com/google/uuid` | v1.6.0 | UUID values | BSD-3-Clause | <https://github.com/google/uuid/tree/v1.6.0> |
| `github.com/jackc/pgx/v5` | v5.10.0 | PostgreSQL protocol client and pool | MIT | <https://github.com/jackc/pgx/tree/v5.10.0> |
| `github.com/pgvector/pgvector-go` | v0.4.1 | pgvector value encoding | MIT; copyright 2021-2026 Andrew Kane | <https://github.com/pgvector/pgvector-go/tree/v0.4.1> |
| `golang.org/x/crypto` | v0.54.0 | Password and cryptographic helpers | BSD-3-Clause | <https://cs.opensource.google/go/x/crypto/+/v0.54.0> |
| `golang.org/x/text` | v0.40.0 | Unicode/text processing | BSD-3-Clause | <https://cs.opensource.google/go/x/text/+/v0.40.0> |

The WebAssembly build in `go-pdfium` embeds a PDFium engine inside the Go
binary. Before the Round-1 binary is distributed, the release SBOM and binary
notice bundle must record the exact embedded PDFium build and preserve any
upstream notices shipped with that build. The Go module version alone does not
prove the embedded engine revision.

`go.mod` and `go.sum` are the authoritative module pins. This file lists
direct runtime dependencies and the embedded PDF engine; the final CycloneDX
SBOM must include all resolved transitive modules, including Wazero and the
embedding runtime chain.
