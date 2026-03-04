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

# Inspect and download
ob1 status                    # compare local files with the vault
ob1 list                      # list all remote files
ob1 cat path/to/note.md       # print a file to stdout
ob1 get path/to/note.md       # download specific files
ob1 get --merge note.md       # merge text changes when possible
ob1 pull --only-notes         # download all .md files
ob1 pull --delete-unknown     # mirror remote deletions for non-hidden files

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
| `status` | Compare local files with the remote vault in a `git status` style view. Use `-a` to include files that are already in sync. |
| `cat <path>` | Print a remote file's contents to stdout. |
| `get <file> [...]` | Download specific files. Skips files that are already up to date locally. Use `--merge` to merge text changes instead of overwriting. |
| `pull` | Download all remote files. Use `--only-notes` to limit to `.md` files, `--merge` to merge text changes, and `--delete-unknown` to remove non-hidden local files that no longer exist remotely. Existing files are backed up before overwrite or deletion unless `--no-backup` is set. |
| `put <file> [...]` | Upload local files to the vault. Skips unchanged files. |

All commands support `--debug` for request and protocol logging, `--no-cache` to bypass the local snapshot cache, and `--dry-run` to show what would change without making local changes. `status` also supports `-v` for human-readable explanations.

## Local state

| Path | Contents |
|---|---|
| `~/.ob1/user.json` | Session token (user-level, shared across vaults) |
| `.ob1/vault.json` | Vault config: ID, host, encryption key, device name, and sync state |
| `.ob1/cache.json` | Cached remote file listing for faster `list` and `status` |
| `.ob1/base/` | Merge bases used by `get --merge` and `pull --merge` |
| `.ob1/backup/` | Backups created by `pull` before overwriting or deleting local files |

All credential files are written with mode 0600.

## Limitations

- **Encrypted vault setup** requires a password-based vault with an exposed salt. Managed-encryption (Obsidian-hosted key) vaults are not yet supported.
- **Merge support is incomplete.** `--merge` only applies to text files, depends on `diff3`, and uses locally stored merge bases. If no base exists yet, `ob1` falls back to standard two-way conflict markers instead of a full three-way merge.
- **Binary conflicts are not merged.** If a binary file changed both locally and remotely, `ob1` overwrites the local copy during `pull`, with a backup by default.
- **There is no continuous sync loop.** Sync is explicit: you run commands such as `pull`, `get`, and `put` yourself.
- **History and restore flows are not exposed yet.** The client does not currently let you browse or recover older remote revisions from the CLI.

## Contributing

Contributions are welcome! For bug fixes and small improvements, go ahead and open a PR. For refactoring or larger changes, please open an issue first to discuss the approach.

AI-assisted contributions are welcome. If you used AI tools, please verify and test all changes yourself before submitting, and mention the models used in your PR description.

## License

MIT. See [LICENSE](LICENSE).
