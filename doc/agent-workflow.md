# Agent Workflow

Use this workflow when an agent needs to read, search, or update notes in an `ob1`-managed vault.

## Refresh Before Searching

Before searching, summarizing, or otherwise reading across the vault, refresh the local note tree first:

```sh
ob1 pull --merge --only-notes
```

This updates all Markdown notes so the agent is working from a current local view of the vault.

If any file now contains conflict markers, stop and resolve them before using those files as source material:

    <<<<<<< local
    =======
    >>>>>>> remote

## Safely Update One Note

When editing a single note such as `path/to/note.md`, use this sequence:

1. Refresh the note before editing:

   ```sh
   ob1 get --merge path/to/note.md
   ```

2. If the file contains conflict markers, resolve them first. Do not continue until they are removed.

3. Edit the file locally.

4. Refresh the same note again immediately before upload:

   ```sh
   ob1 get --merge path/to/note.md
   ```

   This pulls in remote changes that may have happened while the agent was editing.

5. Check again for conflict markers. If any are present, stop and resolve them before upload.

6. Upload only the file that was intentionally changed:

   ```sh
   ob1 put path/to/note.md
   ```

7. Optionally verify the result:

   ```sh
   ob1 status
   ```

   If the file is not listed, it is in sync. To include in-sync files as well:

   ```sh
   ob1 status -a
   ```

## Rules

- Prefer `get --merge` for the specific note being edited.
- Prefer `pull --merge --only-notes` before broad note discovery, searching, or summarization.
- Never run `put` on a file that still contains conflict markers.
- `ob1 put` accepts file paths only. Do not pass directories such as `notes/` or `.`; expand to explicit files first.
- Only run `put` for files the agent intentionally changed.
- If `ob1 get --merge` produces conflicts, treat that as a required resolution step, not as a successful sync.
- Do not use `ob1 experimental mv` on production data. It is experimental and has known sync bugs.
- For rename/move workflows, use manual copy + upload + remove:

```sh
cp old/path.md new/path.md
ob1 put new/path.md
ob1 rm old/path.md
```
