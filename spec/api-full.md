# Obsidian Sync API Reference (Observed Client Protocol)

This document describes the API surface and wire behavior used by the existing Node client in this repository, rewritten as an implementation guide for an independent cleanroom client.

It is intentionally focused on protocol behavior and data flow, not on the internal structure of the current codebase.

## Scope

The observed client uses two remote interfaces:

1. An HTTPS JSON control-plane API on `https://api.obsidian.md`
2. A websocket sync protocol connected directly to the vault host

The HTTP API is used for authentication and vault setup.
The websocket is used for sync state, file metadata, file transfer, history, and related runtime operations.

## Interoperability Summary

To interoperate with the observed service, a client must implement:

1. Authentication token acquisition and storage
2. Vault discovery
3. Password-to-key derivation
4. `keyhash` derivation for the selected encryption version
5. Deterministic metadata encoding/decoding
6. Randomized file-body encryption/decryption
7. Websocket session init
8. `push` replay handling until `ready`
9. `pull(uid)` body transfer
10. `push(...)` metadata and optional binary upload

## Version Support

Observed constants:

- Latest encryption version: `3`
- Highest supported encryption version in this client: `3`

Observed version handling:

- Version `0`: legacy mode
- Versions `2` and `3`: current E2EE modes with deterministic AES-SIV metadata encoding

Version `1` was not observed in the current client.

## Data Encoding Conventions

Observed conventions:

- JSON uses UTF-8
- Hex strings are lowercase
- Binary keys stored locally are base64
- Password and salt strings are normalized with Unicode `NFKC` before key derivation
- Deterministically encrypted metadata is hex-encoded
- Randomly encrypted file content is sent as raw binary frames on the websocket

Timestamps observed on sync objects:

- `ctime` and `mtime` are integer milliseconds since Unix epoch

## HTTP Control-Plane API

Base URL:

- `https://api.obsidian.md`

General request behavior:

- Method: `POST`
- Content-Type: `application/json`
- Request body: JSON object
- Response body: JSON object
- Error shape: if the decoded response contains an `error` field, the client treats it as an application error

Observed client behavior for error handling:

- A JSON response like `{"error":"..."}` is treated as a semantic API failure even if HTTP status is not checked first
- Non-2xx statuses should still be treated as errors

### `POST /user/signin`

Purpose:

- Exchange account credentials for an auth token

Observed request body:

```json
{
  "email": "user@example.com",
  "password": "secret",
  "mfa": "123456"
}
```

Observed extra request behavior:

- The client first sends an `OPTIONS` preflight to the same path
- The `POST` includes `Origin: https://obsidian.md`

Observed response fields:

```json
{
  "token": "string",
  "name": "string",
  "email": "string"
}
```

### `POST /user/signout`

Purpose:

- Invalidate the current auth token server-side

Observed request body:

```json
{
  "token": "string"
}
```

Observed response:

- No specific fields required by the client

### `POST /user/info`

Purpose:

- Validate a token and fetch basic user identity

Observed request body:

```json
{
  "token": "string"
}
```

Observed response fields:

```json
{
  "name": "string",
  "email": "string"
}
```

### `POST /vault/regions`

Purpose:

- List available regions for vault creation or relocation flows

Observed request body:

```json
{
  "token": "string",
  "host": "optional-host-string"
}
```

Observed response fields:

```json
{
  "regions": [
    {
      "value": "us",
      "name": "United States"
    }
  ]
}
```

### `POST /vault/list`

Purpose:

- List the user’s owned and shared vaults

Observed request body:

```json
{
  "token": "string",
  "supported_encryption_version": 3
}
```

Observed response fields:

```json
{
  "vaults": [
    {
      "id": "string",
      "name": "string",
      "region": "string",
      "host": "string",
      "salt": "string",
      "password": "string",
      "encryption_version": 3
    }
  ],
  "shared": [
    {
      "id": "string",
      "name": "string",
      "region": "string",
      "host": "string",
      "salt": "string",
      "password": "string",
      "encryption_version": 3
    }
  ]
}
```

Notes:

- `password` here is not the user’s raw password. It appears to be a server-supplied hint or derived value relevant to shared-vault access flows.
- `salt` is the vault salt used for key derivation.

### `POST /vault/create`

Purpose:

- Create a remote vault

Observed request body:

```json
{
  "token": "string",
  "name": "Vault Name",
  "keyhash": "hex-string-or-null",
  "salt": "hex-string-or-null",
  "region": "string",
  "encryption_version": 3
}
```

Notes:

- For non-E2EE creation, `keyhash` and `salt` may be `null`
- For E2EE creation, both are supplied

Observed response fields:

```json
{
  "id": "string",
  "name": "string",
  "region": "string"
}
```

### `POST /vault/access`

Purpose:

- Prove possession of the correct vault key material for a selected vault

Observed request body:

```json
{
  "token": "string",
  "vault_uid": "vault-id",
  "keyhash": "hex-string",
  "host": "vault-host",
  "encryption_version": 3
}
```

Observed response:

- No specific fields required by the client
- Success is interpreted as “password/key material is valid”

## Key Derivation And Encryption

This is the most important part for a cleanroom implementation. If this layer is wrong, the client will authenticate but fail to decode metadata or file bodies.

## Password To Vault Key

Inputs:

- Password string
- Vault salt string

Normalization:

- Normalize both password and salt with Unicode `NFKC`

KDF:

- Algorithm: `scrypt`
- Parameters:
  - `N = 32768`
  - `r = 8`
  - `p = 1`
  - output length = `32` bytes

Result:

- A 32-byte raw vault key

## `keyhash` Derivation

The websocket `init` and `/vault/access` both require a deterministic `keyhash`.

### Encryption Version 0

Computation:

1. Compute `SHA-256(rawKey)`
2. Hex-encode the 32-byte digest

### Encryption Versions 2 And 3

Computation:

1. Use `HKDF-SHA256`
2. IKM = `rawKey`
3. Salt = UTF-8 bytes of the vault salt string
4. Info = UTF-8 bytes of `"ObsidianKeyHash"`
5. Output length = `32`
6. Hex-encode the result

## Metadata Encryption (Deterministic)

Metadata fields observed as deterministically encoded:

- sync object `path`
- sync object `hash`
- `relatedpath` for rename/history flows

Deterministic means the same plaintext must encode to the same ciphertext every time for the same key material.

### Version 0 Metadata Encoding

Algorithm:

- AES-256-GCM using the raw vault key

Nonce derivation:

1. UTF-8 encode the plaintext string
2. Compute `SHA-256(plaintextBytes)`
3. Take the first 12 bytes as the GCM nonce

Ciphertext layout:

1. 12-byte nonce
2. GCM ciphertext with tag

Transport encoding:

- Hex-encode `nonce || ciphertext`

Decode:

1. Hex-decode
2. Split first 12 bytes as nonce
3. AES-GCM decrypt the remainder

### Versions 2 And 3 Metadata Encoding

Algorithm:

- AES-SIV-style deterministic authenticated encryption
- No associated data was observed

Derived keys:

1. Derive `encKey` via `HKDF-SHA256`
   - IKM = raw vault key
   - Salt = UTF-8 bytes of the vault salt string
   - Info = UTF-8 bytes of `"ObsidianAesSivEnc"`
   - Length = 32 bytes
2. Derive `macKey` via `HKDF-SHA256`
   - IKM = raw vault key
   - Salt = UTF-8 bytes of the vault salt string
   - Info = UTF-8 bytes of `"ObsidianAesSivMac"`
   - Length = 32 bytes

Observed construction:

1. Compute a synthetic IV/tag using S2V/CMAC over the plaintext only
2. Copy the 16-byte tag
3. Clear bit 7 of bytes:
   - `tag[len-8]`
   - `tag[len-4]`
4. Use the modified 16-byte value as the AES-CTR IV
5. AES-CTR encrypt the plaintext
6. Output `tag || ctrCiphertext`

Transport encoding:

- Hex-encode the full binary result

Decode:

1. Hex-decode
2. Split first 16 bytes as tag
3. Derive CTR IV by clearing the same bits
4. AES-CTR decrypt the remainder
5. Recompute S2V on the plaintext
6. Constant-time compare with the tag

Important:

- A standard library AES-SIV implementation may work if it matches RFC 5297 behavior with no associated data
- The observed client derives separate 32-byte encryption and MAC keys and uses a standard S2V + CTR composition

## File-Body Encryption (Randomized)

File bodies use randomized encryption and are transferred separately from metadata.

### Version 0

Algorithm:

- AES-256-GCM using the raw vault key

Ciphertext layout:

1. Random 12-byte IV
2. GCM ciphertext with tag

### Versions 2 And 3

Algorithm:

- AES-256-GCM

Derived content key:

1. Use `HKDF-SHA256`
2. IKM = raw vault key
3. Salt = empty byte string
4. Info = UTF-8 bytes of `"ObsidianAesGcm"`
5. Output = 32-byte AES-GCM key

Ciphertext layout:

1. Random 12-byte IV
2. GCM ciphertext with tag

Observed API behavior:

- On upload, if the plaintext body is non-empty, the client encrypts it before sending binary frames
- On download, after all binary pieces are reassembled, the client decrypts the combined payload

## Websocket Sync Protocol

The sync websocket connects to the vault host, not the control-plane host.

URL construction:

- Use `wss://<host>` for normal hosts
- Use `ws://<host>` for `localhost...` or `127.0.0.1...`

Observed client-side host validation:

- The Node client refuses to connect unless the hostname ends with `.obsidian.md` or is exactly `127.0.0.1`

An independent implementation may choose to be less strict, but this validation is part of the observed client behavior.

## Session Lifecycle

Observed flow:

1. Open websocket
2. Send `init`
3. Wait for auth success response
4. Switch into event mode
5. Receive `push` updates
6. Receive `ready` when initial replay is complete
7. Issue request/response operations like `pull`, `push`, `history`, `deleted`, `restore`

## Heartbeat And Liveness

Observed timing constants in the Node client:

- Heartbeat check interval: `20s`
- If no message for `>10s`, send `{"op":"ping"}`
- If no message for `>2m`, disconnect

Observed server reply:

- `{"op":"pong"}` is treated as a keepalive and otherwise ignored

## `init` Request

First message sent after websocket open:

```json
{
  "op": "init",
  "token": "auth-token",
  "id": "vault-id",
  "keyhash": "hex-string",
  "version": 0,
  "initial": true,
  "device": "device-name",
  "encryption_version": 3
}
```

Field semantics:

- `token`: auth token from `/user/signin`
- `id`: vault ID
- `keyhash`: derived from the vault key and salt
- `version`: client’s current global sync cursor
- `initial`: whether the client still considers itself in initial-sync mode
- `device`: human-readable device label used in history and object metadata
- `encryption_version`: the vault encryption version

## `init` Response

Observed success pattern:

```json
{
  "res": "ok",
  "perFileMax": 123456789,
  "userId": 123
}
```

Observed error pattern:

```json
{
  "status": "err",
  "msg": "message"
}
```

or:

```json
{
  "res": "err",
  "msg": "message"
}
```

Observed optional fields:

- `perFileMax`: maximum allowed file size in bytes for uploads
- `userId`: current user numeric ID

Observed client expectations:

- Any non-`pong` first message must indicate success via `res == "ok"`
- If `perFileMax` is present, it must be a non-negative integer

## Server-Initiated Event Messages

### `push`

Purpose:

- Replay remote state during initial sync
- Deliver live remote changes after initial sync

Observed payload fields:

```json
{
  "op": "push",
  "uid": 123,
  "path": "hex-deterministic-ciphertext",
  "size": 42,
  "hash": "hex-deterministic-ciphertext",
  "ctime": 1700000000000,
  "mtime": 1700000001000,
  "folder": false,
  "deleted": false,
  "device": "Laptop",
  "user": 123,
  "relatedpath": "optional-hex-deterministic-ciphertext"
}
```

Client behavior:

1. Deterministically decode `path`
2. If `hash` is non-empty, deterministically decode `hash`
3. Treat `uid` as both object ID and sync cursor update
4. During initial replay, collect `push` objects until `ready`

Notes:

- `relatedpath` is present in the observed schema, likely for rename or history-related semantics
- The current listing command in the Go port does not use `relatedpath`

### `ready`

Purpose:

- Marks the end of initial replay

Observed payload:

```json
{
  "op": "ready",
  "version": 123
}
```

Client behavior:

- Set local `initial = false`
- Advance stored sync version to at least `version`

### `pong`

Purpose:

- Keepalive reply

Observed payload:

```json
{
  "op": "pong"
}
```

## Client Request/Response Operations

Outside of server-pushed `push` and `ready` events, the websocket behaves like a single in-flight request/response channel plus an out-of-band binary channel for file data.

Observed model:

- One JSON request at a time
- One JSON response resolves that request
- Binary frames belong to the active request unless consumed as queued data

This is important: the observed client serializes these operations through an internal queue.

## `pull`

Purpose:

- Download a file body by remote object `uid`

Observed request:

```json
{
  "op": "pull",
  "uid": 123
}
```

Observed first response fields:

```json
{
  "deleted": false,
  "size": 1048576,
  "pieces": 1
}
```

Observed client behavior:

1. Send `pull`
2. Wait for a JSON response
3. If `deleted == true`, return `null` and read no binary frames
4. Otherwise allocate `size` bytes
5. Read exactly `pieces` binary frames
6. Concatenate them in order
7. If total byte length is non-zero, decrypt as a file body
8. Return plaintext bytes

Inferred constraints:

- `pieces` is authoritative for frame count
- `size` is the total encrypted size, not necessarily plaintext size

## `push`

Purpose:

- Create/update/delete files and folders

Observed common fields:

- `path`: deterministically encoded logical path
- `relatedpath`: deterministically encoded previous path for rename-like updates, or `null`
- `extension`: file extension without dot for files, `""` for folders
- `hash`: deterministically encoded plaintext SHA-256 file hash for files, `""` for folders/deletes
- `ctime`
- `mtime`
- `folder`
- `deleted`

### Folder Create / Metadata-Only Delete

Observed request shape:

```json
{
  "op": "push",
  "path": "hex",
  "relatedpath": null,
  "extension": "",
  "hash": "",
  "ctime": 0,
  "mtime": 0,
  "folder": true,
  "deleted": false
}
```

For file delete:

```json
{
  "op": "push",
  "path": "hex",
  "relatedpath": null,
  "extension": "md",
  "hash": "",
  "ctime": 0,
  "mtime": 0,
  "folder": false,
  "deleted": true
}
```

Observed behavior:

- No binary frames follow

### File Upload

Observed client algorithm:

1. Compute plaintext SHA-256 of the file body
2. Hex-encode the digest
3. Deterministically encode that hash string
4. If body length > 0, encrypt the file body using randomized AES-GCM
5. Split encrypted payload into pieces of `2 MiB` (`2 * 1024 * 1024`)
6. Send metadata `push`
7. Inspect the JSON response
8. If the response has `res == "ok"`, stop: no binary upload was required
9. Otherwise send all binary pieces, one frame at a time
10. After each binary frame, wait for a JSON response before sending the next

Observed metadata request shape:

```json
{
  "op": "push",
  "path": "hex",
  "relatedpath": "hex-or-null",
  "extension": "md",
  "hash": "hex",
  "ctime": 1700000000000,
  "mtime": 1700000001000,
  "folder": false,
  "deleted": false,
  "size": 12345,
  "pieces": 1
}
```

Notes:

- `size` is the encrypted payload length
- `pieces` is the number of binary frames that will follow if the server requires them

Important inferred behavior:

- The metadata response is a server-side short-circuit check. The server may already have the same content and can accept the metadata immediately with `res == "ok"`.
- Any non-`ok` metadata response is treated by the observed client as “continue with binary upload.”
- The exact non-`ok` success code used by the server for “send pieces now” was not directly confirmed by the current client code.

## `deleted`

Purpose:

- List deleted items (tombstones)

Observed request:

```json
{
  "op": "deleted",
  "suppressrenames": true
}
```

Observed response shape:

```json
{
  "items": [
    {
      "path": "hex"
    }
  ]
}
```

Observed client behavior:

- Deterministically decode each item `path`

Additional tombstone fields may exist, but only `path` is required by the current code path.

## `history`

Purpose:

- Retrieve version history for a logical path

Observed request:

```json
{
  "op": "history",
  "path": "hex",
  "last": 50
}
```

Observed response shape:

```json
{
  "items": [
    {
      "path": "hex",
      "relatedpath": "hex-or-null"
    }
  ]
}
```

Observed client behavior:

- Deterministically decode `path`
- If present, deterministically decode `relatedpath`

Other history fields likely exist, but only those fields are explicitly used in the observed client method.

## `restore`

Purpose:

- Restore a historical object by `uid`

Observed request:

```json
{
  "op": "restore",
  "uid": 123
}
```

Observed response:

- Not further decoded by the current client wrapper

## `usernames`

Purpose:

- Fetch a mapping of sync user IDs to names

Observed request:

```json
{
  "op": "usernames"
}
```

Observed response:

- Not further decoded by the current client wrapper

## `purge`

Purpose:

- Trigger a server-side purge/maintenance action

Observed request:

```json
{
  "op": "purge"
}
```

Observed response:

- Not further decoded by the current client wrapper

## `size`

Purpose:

- Query remote storage usage

Observed request:

```json
{
  "op": "size"
}
```

Observed response:

- Not further decoded by the current client wrapper

## Global Sync State Model

A compatible client should persist at least:

1. Global sync version cursor
2. Whether the session is still in initial-sync mode
3. Last known local file metadata
4. Last known remote file metadata
5. Pending remote `push` objects not yet reconciled

Observed Node behavior:

- `uid` from incoming `push` is treated as the advancing sync cursor
- During initial replay, deleted pushes may only advance the cursor
- After `ready`, the client marks `initial = false`
- The client only advances the stored version after it has accepted and recorded the corresponding remote state transition

## File Identity And Hashes

Observed hash behavior for files:

1. Compute SHA-256 over plaintext file bytes
2. Hex-encode the digest
3. Persist that hex string locally
4. Deterministically encode that hex string for remote `push.hash`
5. Deterministically decode remote `push.hash` back into the same plaintext hex string

This means:

- The remote hash metadata is not a hash of ciphertext
- It is a deterministically encrypted wrapper around the plaintext SHA-256 hex digest

## Practical Implementation Flow

A minimal interoperable one-shot client should do this:

1. `POST /user/signin`
2. `POST /vault/list`
3. Pick a vault and derive `rawKey`
4. Compute `keyhash`
5. `POST /vault/access`
6. Connect websocket to the vault host
7. Send `init`
8. Read `push` events until `ready`
9. Decode each `push.path` and `push.hash`
10. Persist `version` and remote object index
11. To fetch one file, send `pull(uid)`, collect `pieces`, decrypt, write plaintext

For upload support:

1. Compute plaintext SHA-256
2. Deterministically encode path and hash
3. Encrypt the body
4. Send metadata `push`
5. If the server does not short-circuit with `res == "ok"`, stream binary pieces with per-piece ACK handling

## Known Uncertainties

The following behaviors are strongly inferred from the observed client, but not fully confirmed from server documentation:

1. The exact JSON response payload used by the server to request binary upload after a metadata `push`
2. The full field set returned by `deleted`, `history`, `usernames`, `purge`, and `size`
3. Whether version `2` and version `3` differ on the server beyond both using the same client-side crypto handling in the current implementation
4. The precise semantics of `relatedpath` across all server operations

These are implementation risks, but they do not block a working minimal client for login, vault access, snapshot replay, metadata decode, and `pull(uid)`.

## Recommended Cleanroom Validation Order

To validate an independent implementation, test in this order:

1. Reproduce `scrypt` output for known password/salt inputs
2. Reproduce `keyhash` for version `0` and version `3`
3. Round-trip deterministic metadata encoding/decoding for a known path string
4. Authenticate with `/vault/access`
5. Open websocket and complete `init`
6. Decode a remote snapshot from `push` + `ready`
7. Successfully `pull(uid)` and decrypt one file body
8. Only then implement `push(...)` uploads and mutation flows

## Relationship To `doc/api.md`

[`api.md`](/home/bot/src/obsidian-headless/doc/api.md) is the short operational overview.

This file is the protocol-oriented reference for building another client from scratch.
