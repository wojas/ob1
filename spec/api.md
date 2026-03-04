# Obsidian API Flow

This project talks to two different remote interfaces:

1. A small HTTPS JSON API on `https://api.obsidian.md` for account and vault setup.
2. A websocket-based sync protocol on the vault host for live object replication.

The important thing to understand is that the HTTP API gets you authenticated and configured, while the websocket protocol is where the actual vault state moves.

## Mental Model

At a high level, the client does this:

1. Authenticate the user and store a bearer-like auth token locally.
2. Query the control-plane API to discover vaults and their metadata.
3. Derive or validate the vault encryption key locally from the user password.
4. Open a websocket directly to the vault host and identify itself with:
   - the auth token
   - the vault ID
   - a deterministic `keyhash` derived from the encryption key
   - the client’s current sync version
5. Receive a stream of remote object updates (`push` messages) until the server sends `ready`.
6. After that initial snapshot, continue exchanging file changes over the websocket.

That split matters because most debugging issues fall cleanly into one of two buckets:

- HTTP/setup failures: login, vault discovery, password validation.
- Sync/runtime failures: websocket auth, missing objects, stale state, bad merge decisions.

## Phase 1: Login And Token Storage

Login is a normal HTTPS POST to `/user/signin`.

The client sends:

- `email`
- `password`
- `mfa`

If successful, the server returns a token plus basic user info. That token is then stored locally and reused for later API calls. Logout is another POST to `/user/signout`, but the client also removes the local token file even if the remote signout call fails.

This token is the main credential for both the HTTP control-plane API and the websocket sync handshake.

## Phase 2: Vault Discovery And Setup

Before sync starts, the client needs enough metadata to reach the correct vault host and prove it has the correct encryption password.

The basic setup flow is:

1. Call `/vault/list` with the auth token and the highest encryption version the client supports.
2. Choose a vault from the returned `vaults` and `shared` lists.
3. Read the vault metadata:
   - vault ID
   - vault name
   - host
   - region
   - salt
   - encryption version
4. Derive the raw 32-byte vault key from the user’s password and the vault salt using `scrypt`.
5. Derive a deterministic `keyhash` from that raw key.
6. Call `/vault/access` with:
   - token
   - vault ID
   - keyhash
   - host
   - encryption version
7. If `/vault/access` succeeds, persist the local vault config and the encoded encryption key.

The server never needs the raw encryption key itself. The password is turned into a key locally, and the server only sees the derived `keyhash` as proof that the client has the right password.

## Phase 3: Creating A New Remote Vault

Remote vault creation is still a control-plane API action.

The flow is:

1. Optionally ask the user for an end-to-end encryption password.
2. If E2EE is enabled:
   - generate a random salt
   - derive the raw 32-byte key with `scrypt`
   - compute the `keyhash`
3. POST to `/vault/create` with:
   - token
   - vault name
   - optional `keyhash`
   - optional salt
   - region
   - encryption version

If the vault is created with managed encryption instead of E2EE, the `keyhash` and salt are omitted.

## Phase 4: Websocket Sync Handshake

Once a vault is configured, the client stops talking to `api.obsidian.md` for the actual sync stream and instead connects to the vault host:

- `wss://<host>` in normal use
- `ws://<host>` only for local development hosts like `127.0.0.1` or `localhost`

The first websocket message is always an `init` JSON object containing:

- `op: "init"`
- `token`
- `id` (vault ID)
- `keyhash`
- `version` (the client’s known sync cursor)
- `initial` (whether this is the first sync)
- `device`
- `encryption_version`

The server replies with an initial auth result. If that succeeds, the connection stays open and the server starts sending sync events.

The key point is that websocket auth is not just “token is valid.” The server also checks that the vault ID, supported encryption version, and `keyhash` line up.

## Phase 5: Initial Snapshot

After the websocket handshake succeeds, the server emits a stream of `push` messages representing remote objects, followed by a final `ready` message.

You can think of it like this:

1. `init` authenticates the session.
2. `push` messages replay the current remote state (and can also represent live changes).
3. `ready` tells the client that the initial replay is complete and includes the latest global sync version.

Each `push` message carries object metadata such as:

- `uid`
- encrypted `path`
- encrypted `hash` for files
- `size`
- `ctime`
- `mtime`
- `folder`
- `deleted`
- device/user metadata

The path and hash are not sent in plaintext. They must be deterministically decrypted locally using the vault encryption key.

This is why a remote object listing can be implemented by:

1. connecting with `version = 0` and `initial = true`
2. collecting `push` objects
3. decoding the encrypted path/hash
4. stopping once `ready` arrives

That gives you a point-in-time remote snapshot without downloading file bodies.

## Phase 6: File Bodies

Metadata and file contents travel separately.

The websocket `push` stream gives object metadata, but actual file bytes are fetched with a separate websocket request:

- `op: "pull"`
- `uid`

The server responds with:

1. a JSON metadata response telling the client how many binary pieces follow
2. one or more binary frames containing the file body

If the file is encrypted, the client decrypts the assembled payload locally after all pieces are received.

This split is important when debugging “missing attachments”:

- if the object appears in the snapshot but content download fails, the metadata path is working and the problem is in `pull`
- if the object never appears at all, the issue is earlier: filtering, remote state, or deterministic decode

## Phase 7: Uploads And Deletes

Uploads also happen over the websocket.

The client sends `op: "push"` for several distinct cases:

- create folder
- delete folder
- delete file
- create or update file metadata, followed by binary chunks if file content must be sent

For a normal file upload, the client:

1. deterministically encrypts the logical path
2. deterministically encrypts the file content hash
3. encrypts the file body itself if the vault uses E2EE
4. sends a `push` metadata frame with:
   - encrypted path
   - related path for renames when present
   - file extension
   - encrypted content hash
   - timestamps
   - size
   - piece count
5. if the server expects file bytes, stream the binary pieces afterward

Deletes are represented as `push` operations with `deleted = true`.

## Deterministic vs Random Encryption

The protocol uses two different encryption styles for different jobs.

Deterministic encryption is used for metadata fields that the server needs to compare or index:

- object path
- file hash

That lets the server recognize “same logical path” or “same content hash” without seeing the plaintext.

Randomized encryption is used for file content payloads:

- file bytes are encrypted with a nonce/IV so identical files do not produce identical ciphertext

In practice:

- metadata must be stable across repeated encodes
- file bodies must not be stable across repeated encodes

## How Versioning Works

The client stores a sync version cursor locally. That version is sent in the websocket `init` message.

The server then uses that version to decide which changes the client needs. Once the server sends `ready`, it includes the latest version number. The client updates its local stored version so the next sync can resume from there.

This version is global sync state, not a per-file revision number.

That is why a stale or corrupted local state store can cause confusing behavior:

- too old: the client replays more remote changes than expected
- too new: the client can appear to miss remote changes

## Debugging By Flow

When something breaks, debug in this order:

1. Token layer:
   - can the client call `/user/info`?
   - if not, the auth token is invalid or missing
2. Control-plane layer:
   - can `/vault/list` and `/vault/access` succeed?
   - if not, the vault selection, password, or key derivation is wrong
3. Websocket handshake:
   - does `init` succeed?
   - if not, token, vault ID, keyhash, host, or encryption version is wrong
4. Snapshot layer:
   - do `push` objects arrive before `ready`?
   - if not, the connection is established but no state is being replayed
5. Decode layer:
   - can encrypted paths/hashes be deterministically decoded?
   - if not, the local crypto implementation is wrong
6. Content layer:
   - does `pull(uid)` return the expected binary pieces?
   - if not, the object exists but body transfer is failing

This ordering isolates most failures quickly.

## Endpoint Summary

The project currently relies on these HTTPS endpoints:

- `/user/signin`
- `/user/signout`
- `/user/info`
- `/vault/regions`
- `/vault/list`
- `/vault/create`
- `/vault/access`

And these websocket sync operations:

- `init`
- `ping` / `pong`
- `push`
- `pull`
- `deleted`
- `history`
- `restore`
- `usernames`
- `purge`
- `size`

The HTTP endpoints establish identity and vault configuration. The websocket operations are the real data plane.

