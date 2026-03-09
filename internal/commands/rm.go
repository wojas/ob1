package commands

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"

	"github.com/spf13/cobra"

	"github.com/wojas/ob1/internal/remotelist"
	"github.com/wojas/ob1/internal/vaultstore"
)

func NewRemoveCommand(rt Runtime, debug *bool, noCache *bool) *cobra.Command {
	var noBackup bool

	cmd := &cobra.Command{
		Use:   "rm <file1> [file2] [...]",
		Short: "Remove remote files from the vault",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			logger := rt.NewLogger(*debug)
			backup := newBackupSession(!noBackup)

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

			targets := make([]string, 0, len(args))
			seen := make(map[string]struct{}, len(args))
			for _, arg := range args {
				targetPath, ok := safeLocalTarget(arg)
				if !ok {
					logger.Warn("skipping dangerous path", "path", arg)
					continue
				}

				targetPath = filepath.ToSlash(targetPath)
				if _, exists := seen[targetPath]; exists {
					continue
				}
				seen[targetPath] = struct{}{}
				targets = append(targets, targetPath)
			}

			if len(targets) == 0 {
				logger.Warn("no safe files to remove")
				return nil
			}
			sort.Strings(targets)

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

			for _, target := range targets {
				entry, exists := entriesByPath[target]
				if !exists {
					return fmt.Errorf("remote file %q not found", target)
				}
				if entry.Folder {
					return fmt.Errorf("%q is a folder; rm currently supports files only", target)
				}
			}

			if rt.IsDryRun() {
				for _, target := range targets {
					logger.Info("would remove remote file", "path", target)
					if err := removeLocalFile(logger, filepath.FromSlash(target), backup, true); err != nil {
						return err
					}
				}
				return nil
			}

			updated, err := remotelist.RemoveFiles(cmd.Context(), logger, userState.Token, vaultState, targets, snapshot)
			if err != nil {
				return err
			}

			for _, target := range targets {
				localPath := filepath.FromSlash(target)
				if err := removeLocalFile(logger, localPath, backup, false); err != nil {
					return err
				}

				if err := removeMergeBase(localPath); err != nil {
					return err
				}
			}

			if err := maybeSaveRemoteCache(cacheStore, nil, updated, effectiveNoCache(*noCache, rt.IsDryRun())); err != nil {
				return err
			}

			return nil
		},
	}

	cmd.Flags().BoolVar(&noBackup, "no-backup", false, "do not back up local files before removing them")

	return cmd
}

func removeLocalFile(logger *slog.Logger, path string, backup *backupSession, dryRun bool) error {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		logger.Debug("local file already absent", "path", path)
		return nil
	case err != nil:
		return fmt.Errorf("lstat %s: %w", path, err)
	}

	if info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("%s is a directory", path)
	}

	if err := ensureNoSymlinkAncestor(path); err != nil {
		return err
	}

	if dryRun {
		if backup != nil {
			logger.Warn("would remove local file", "path", path, "backup", backup.target(path))
		} else {
			logger.Warn("would remove local file without backup", "path", path)
		}
		return nil
	}

	if backup != nil {
		backupPath, err := backup.move(path)
		if err != nil {
			return err
		}
		logger.Info("removed local file", "path", path, "backup", backupPath)
		return nil
	}

	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	logger.Warn("removed local file without backup", "path", path)

	return nil
}
