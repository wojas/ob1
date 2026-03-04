<p align="center">
  <img src="doc/assets/ob1-256.png" alt="ob1 logo" width="220">
</p>

# ob1

A command-line client for [Obsidian Sync](https://obsidian.md/sync) that lets you download, upload, and inspect vault contents without the Obsidian app.

Built in Go. Supports end-to-end encrypted vaults.

## Installation

```sh
go install github.com/wojas/ob1/cmd/ob1@latest
```

Requires Go 1.25 or later.

## Quick start

```sh
# Authenticate (interactive prompt for email/password/MFA)
ob1 login

# List your vaults and pick one to set up locally
ob1 vault list
ob1 vault setup <vault-id>

# Browse and download
ob1 list                      # list all remote files
ob1 cat path/to/note.md       # print a file to stdout
ob1 get path/to/note.md       # download specific files
ob1 pull --only-notes         # download all .md files

# Upload
ob1 put path/to/note.md       # upload specific files
```

Running `ob1 vault setup` in a directory creates a `.ob1/` folder that ties that directory to your vault. All subsequent commands operate relative to this directory.

## Commands

| Command | Description |
|---|---|
| `login` | Sign in to your Obsidian account. Accepts `--email`, `--password`, `--mfa` flags or prompts interactively. |
| `logout` | Sign out and delete the local session token. |
| `info` | Print account metadata. |
| `vault list` | List all vaults you own or have access to. |
| `vault setup <id>` | Configure the current directory for a specific vault. Prompts for your vault encryption password. |
| `list` | List all files in the remote vault. Use `--cached` to read from the local cache without contacting the server. |
| `cat <path>` | Print a remote file's contents to stdout. |
| `get <file> [...]` | Download specific files. Skips files that are already up to date locally. |
| `pull` | Download all remote files. Use `--only-notes` to limit to `.md` files. Warns before overwriting local changes. |
| `put <file> [...]` | Upload local files to the vault. Skips unchanged files. |

All commands support `--debug` / `-v` for verbose output and `--no-cache` to bypass the local snapshot cache.

## How it works

`ob1` implements the Obsidian Sync protocol directly:

- **Control plane** -- HTTPS calls to `api.obsidian.md` for authentication, vault discovery, and key verification.
- **Data plane** -- WebSocket connection to the vault host for file listing, downloading, and uploading.

Encryption is handled client-side. The server never sees your password or raw encryption key -- only a derived key hash. File paths and contents are encrypted with AES-GCM or AES-SIV depending on the vault's encryption version.

## Local state

| Path | Contents |
|---|---|
| `~/.ob1/user.json` | Session token (user-level, shared across vaults) |
| `.ob1/vault.json` | Vault config: ID, host, encryption key, sync cursor |
| `.ob1/cache.json` | Cached remote file listing for faster `list --cached` |

All credential files are written with mode 0600.

## Limitations

- **Encrypted vault setup** requires a password-based vault with an exposed salt. Managed-encryption (Obsidian-hosted key) vaults are not yet supported.
- **No merge flow.** When both local and remote versions have changed, `pull` picks the remote version after warning. There is no three-way merge.
- **Remote deletions are not mirrored.** Files deleted on the server are not removed locally.
- **One-way operations only.** There is no continuous sync loop -- you explicitly `pull` and `put` files.

## License

MIT. See [LICENSE](LICENSE).
