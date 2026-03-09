package commands

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wojas/ob1/internal/remotelist"
	"github.com/wojas/ob1/internal/vaultstore"
)

func NewMoveCommand(rt Runtime, debug *bool, noCache *bool) *cobra.Command {
	var noBackup bool

	cmd := &cobra.Command{
		Use:   "mv <source> <destination>",
		Short: "Move or rename a remote file and its local copy",
		Long: strings.TrimSpace(`
EXPERIMENTAL: known sync bugs may duplicate or resurrect files.
Do not use this command on production data.

For agent workflows, prefer:
1. copy the file locally to the new path,
2. "ob1 put" the new path,
3. "ob1 rm" the old path.

Move or rename a file in the remote vault and then move the local file.

The destination must be a file path (include a filename). Passing a directory
path such as "archive/" is rejected. To move into another directory, provide
the full destination path, for example "archive/note.md".

Folder moves are not supported.
		`),
		Example: strings.TrimSpace(`
  ob1 experimental mv note.md archive/note.md
  ob1 experimental mv notes/todo.md done/todo-2026-03-09.md
  ob1 --dry-run experimental mv note.md archive/note.md
`),
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			logger := rt.NewLogger(*debug)
			backup := newBackupSession(!noBackup)

			if looksLikeDirectoryPathArg(args[0]) {
				return fmt.Errorf("source must be a file path, got directory-like path %q", args[0])
			}
			if looksLikeDirectoryPathArg(args[1]) {
				return fmt.Errorf("destination must include a file name, got directory-like path %q", args[1])
			}

			sourcePath, ok := safeLocalTarget(args[0])
			if !ok {
				return fmt.Errorf("invalid source path %q", args[0])
			}
			targetPath, ok := safeLocalTarget(args[1])
			if !ok {
				return fmt.Errorf("invalid destination path %q", args[1])
			}
			sourcePath = filepath.ToSlash(sourcePath)
			targetPath = filepath.ToSlash(targetPath)
			if sourcePath == targetPath {
				return errors.New("source and destination must be different")
			}

			userState, err := rt.Store.Load()
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return errors.New("no local session found; login first")
				}
				return err
			}

			vaultState, err := vaultstore.NewInDir(".").Load()
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return errors.New("no local vault config found; run `ob1 vault setup <id>` first")
				}
				return err
			}

			cacheStore := remotelist.NewCacheStore(".")
			cached, err := loadRemoteCache(cacheStore, *noCache)
			if err != nil {
				return err
			}

			snapshot, err := remotelist.SyncEntries(cmd.Context(), logger, userState.Token, vaultState, cached, !*noCache)
			if err != nil {
				return err
			}

			entriesByPath := make(map[string]remotelist.Entry, len(snapshot.Entries))
			for _, entry := range snapshot.Entries {
				entriesByPath[entry.Path] = entry
			}

			source, exists := entriesByPath[sourcePath]
			if !exists {
				return fmt.Errorf("remote file %q not found", sourcePath)
			}
			if source.Folder {
				return fmt.Errorf("%q is a folder; mv currently supports files only", sourcePath)
			}
			if existing, exists := entriesByPath[targetPath]; exists {
				if existing.Folder {
					return fmt.Errorf("remote path %q exists as a folder", targetPath)
				}
				return fmt.Errorf("remote path %q already exists", targetPath)
			}

			if err := preflightLocalMove(filepath.FromSlash(sourcePath), filepath.FromSlash(targetPath)); err != nil {
				return err
			}

			if rt.IsDryRun() {
				logger.Info("would move remote file", "from", sourcePath, "to", targetPath)
				if err := logDryRunLocalMove(logger, filepath.FromSlash(sourcePath), filepath.FromSlash(targetPath), backup); err != nil {
					return err
				}
				if err := logDryRunMergeBaseMove(logger, filepath.FromSlash(sourcePath), filepath.FromSlash(targetPath)); err != nil {
					return err
				}
				return nil
			}

			updated, err := remotelist.MoveFile(cmd.Context(), logger, userState.Token, vaultState, sourcePath, targetPath, snapshot)
			if err != nil {
				return err
			}

			localSource := filepath.FromSlash(sourcePath)
			localTarget := filepath.FromSlash(targetPath)
			if err := moveLocalFile(logger, localSource, localTarget, backup); err != nil {
				return fmt.Errorf("remote move succeeded but local move failed: %w", err)
			}
			if err := moveMergeBase(localSource, localTarget); err != nil {
				return fmt.Errorf("remote move succeeded but merge-base move failed: %w", err)
			}

			if err := maybeSaveRemoteCache(cacheStore, nil, updated, effectiveNoCache(*noCache, rt.IsDryRun())); err != nil {
				return err
			}

			return nil
		},
	}

	cmd.Flags().BoolVar(&noBackup, "no-backup", false, "do not back up overwritten local destination files")

	return cmd
}

func preflightLocalMove(source string, target string) error {
	sourceInfo, err := os.Lstat(source)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return fmt.Errorf("lstat %s: %w", source, err)
	default:
		if sourceInfo.IsDir() && sourceInfo.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("%s is a directory", source)
		}
		if err := ensureNoSymlinkAncestor(source); err != nil {
			return err
		}
	}

	targetInfo, err := os.Lstat(target)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return fmt.Errorf("lstat %s: %w", target, err)
	default:
		if targetInfo.IsDir() && targetInfo.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("%s is a directory", target)
		}
		if err := ensureNoSymlinkAncestor(target); err != nil {
			return err
		}
	}

	if err := ensureNoSymlinkAncestorAllowMissing(target); err != nil {
		return err
	}

	return nil
}

func ensureNoSymlinkAncestorAllowMissing(path string) error {
	dir := filepath.Dir(path)
	if dir == "." {
		return nil
	}

	current := ""
	for _, part := range strings.Split(dir, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		if current == "" {
			current = part
		} else {
			current = filepath.Join(current, part)
		}

		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("lstat %s: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to write through symlinked directory %s", current)
		}
	}

	return nil
}

func logDryRunLocalMove(logger *slog.Logger, source string, target string, backup *backupSession) error {
	info, err := os.Lstat(target)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return fmt.Errorf("lstat %s: %w", target, err)
	default:
		if backup != nil {
			logger.Warn("would overwrite local destination", "path", target, "backup", backup.target(target))
		} else {
			logger.Warn("would overwrite local destination without backup", "path", target)
		}
		if info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("%s is a directory", target)
		}
	}

	sourceInfo, err := os.Lstat(source)
	switch {
	case errors.Is(err, os.ErrNotExist):
		logger.Debug("local source file already absent", "path", source)
		return nil
	case err != nil:
		return fmt.Errorf("lstat %s: %w", source, err)
	}

	if sourceInfo.IsDir() && sourceInfo.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("%s is a directory", source)
	}
	logger.Info("would move local file", "from", source, "to", target)

	return nil
}

func moveLocalFile(logger *slog.Logger, source string, target string, backup *backupSession) error {
	targetInfo, err := os.Lstat(target)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return fmt.Errorf("lstat %s: %w", target, err)
	default:
		if targetInfo.IsDir() && targetInfo.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("%s is a directory", target)
		}
		if backup != nil {
			backupPath, err := backup.move(target)
			if err != nil {
				return err
			}
			logger.Warn("overwriting local destination", "path", target, "backup", backupPath)
		} else {
			logger.Warn("overwriting local destination without backup", "path", target)
			if err := os.Remove(target); err != nil {
				return fmt.Errorf("remove %s: %w", target, err)
			}
		}
	}

	sourceInfo, err := os.Lstat(source)
	switch {
	case errors.Is(err, os.ErrNotExist):
		logger.Debug("local source file already absent", "path", source)
		return nil
	case err != nil:
		return fmt.Errorf("lstat %s: %w", source, err)
	}
	if sourceInfo.IsDir() && sourceInfo.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("%s is a directory", source)
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("create directories for %s: %w", target, err)
	}
	if err := os.Rename(source, target); err != nil {
		return fmt.Errorf("move %s to %s: %w", source, target, err)
	}

	logger.Info("moved local file", "from", source, "to", target)

	return nil
}

func logDryRunMergeBaseMove(logger *slog.Logger, source string, target string) error {
	if source == target {
		return nil
	}

	_, sourceHasBase, err := readMergeBase(source)
	if err != nil {
		return err
	}
	_, targetHasBase, err := readMergeBase(target)
	if err != nil {
		return err
	}

	switch {
	case sourceHasBase && targetHasBase:
		logger.Info("would replace merge base", "from", source, "to", target)
	case sourceHasBase:
		logger.Info("would move merge base", "from", source, "to", target)
	case targetHasBase:
		logger.Info("would remove stale merge base", "path", target)
	}

	return nil
}

func moveMergeBase(source string, target string) error {
	if source == target {
		return nil
	}

	body, hasBase, err := readMergeBase(source)
	if err != nil {
		return err
	}
	if err := removeMergeBase(target); err != nil {
		return err
	}
	if !hasBase {
		return nil
	}
	if _, err := writeMergeBase(target, body); err != nil {
		return err
	}

	return removeMergeBase(source)
}

func looksLikeDirectoryPathArg(value string) bool {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return false
	}

	return strings.HasSuffix(trimmed, "/") || strings.HasSuffix(trimmed, "\\")
}
