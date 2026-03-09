# Open Questions

This file tracks protocol or behavior details that are unclear from available
documentation and observed traffic.

## 2026-03-09

1. `push` rename semantics with `relatedpath` and missing parent folders
- Context: `mv` uses metadata-only `push` with `path` (new path) and `relatedpath` (old path).
- Unclear: If the destination parent folder does not already exist remotely, does the server:
  - auto-create missing parent folders, or
  - reject the rename until folder-create `push` operations are sent first?
- Current client behavior: proactively creates missing destination parent folders before rename.
- Why this matters: impacts interoperability and whether rename is atomic vs multi-step across clients.

2. `push` rename validation fields (`size` and `pieces`) for metadata-only moves
- Context: On a real server, rename via `relatedpath` returned `{"err":"Bad request (size)"}` when `size` was omitted.
- Context (continued): after adding `size`, server returned `{"err":"Bad request (pieces)"}` until `pieces` was also sent.
- Unclear: Are `size` and `pieces` required for all file `push` operations including metadata-only rename across all server versions/regions?
- Current client behavior: includes both `size` and `pieces` for rename pushes.
- Why this matters: required-field differences can cause cross-client rename failures even when paths and hashes are valid.

3. `relatedpath` rename semantics: move vs copy+delete
- Context: A real-server rename request with `path` + `relatedpath` created the new path, but a subsequent pull restored the original name unless the original path was explicitly deleted.
- Unclear: Is `relatedpath` intended to atomically move entries, or to copy metadata/body to a new path while requiring a separate delete of the old path?
- Current client behavior: after rename push, sends an explicit delete push for the source path.
- Why this matters: without explicit source deletion, clients can appear to rename successfully but revert after sync.
