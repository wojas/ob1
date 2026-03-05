package commands

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"

	"github.com/wojas/ob1/internal/remotelist"
	"github.com/wojas/ob1/internal/vaultcrypto"
	"github.com/wojas/ob1/internal/vaultstore"
)

func NewPullCommand(rt Runtime, debug *bool, noCache *bool) *cobra.Command {
	var merge bool
	var onlyNotes bool
	var deleteUnknown bool
	var noBackup bool

	cmd := &cobra.Command{
		Use:   "pull",
		Short: "Fetch all remote files into the current directory",
		RunE: func(cmd *cobra.Command, _ []string) error {
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

			cacheStore := remotelist.NewCacheStore(".")
			cached, err := loadRemoteCache(cacheStore, *noCache)
			if err != nil {
				return err
			}

			stopSyncProgress := startPullProgressTicker(cmd.Context(), logger, func() []any {
				return []any{"stage", "sync-snapshot"}
			})
			snapshot, err := remotelist.SyncEntries(cmd.Context(), logger, userState.Token, vaultState, cached, !*noCache)
			stopSyncProgress()
			if err != nil {
				return err
			}

			if err := maybeSaveRemoteCache(cacheStore, cached, snapshot, effectiveNoCache(*noCache, rt.IsDryRun())); err != nil {
				return err
			}

			pathsToFetch := make([]string, 0, len(snapshot.Entries))
			alreadyUpToDate := 0
			metadataUpdated := 0
			basesCopied := 0
			scannedEntries := 0
			nextPlanProgress := time.Now().Add(5 * time.Second)
			for _, entry := range snapshot.Entries {
				scannedEntries++
				if !entry.Folder && (!onlyNotes || strings.EqualFold(filepath.Ext(entry.Path), ".md")) {
					targetPath, ok := safeLocalTarget(entry.Path)
					if !ok {
						logger.Warn("skipping dangerous path", "path", entry.Path)
					} else {
						upToDate, metadataOnly, err := localFileMatchesRemote(targetPath, entry, !rt.IsDryRun(), rt.IsVerify())
						if err != nil {
							return err
						}
						if upToDate {
							if merge && !rt.IsDryRun() && entry.Hash != "" {
								baseCopied, err := ensureMergeBaseFromLocal(targetPath, entry.Hash)
								if err != nil {
									return err
								}
								if baseCopied {
									basesCopied++
								}
							}
							if metadataOnly {
								metadataUpdated++
								if rt.IsDryRun() {
									logger.Debug("file metadata would be updated", "path", targetPath)
								} else {
									logger.Debug("updated file metadata", "path", targetPath)
								}
							} else {
								alreadyUpToDate++
								logger.Debug("file already up to date", "path", targetPath)
							}
						} else {
							keepLocal := false
							if merge && entry.Hash != "" {
								baseBody, hasBase, err := readMergeBase(targetPath)
								if err != nil {
									return err
								}
								if hasBase && vaultcrypto.PlaintextHash(baseBody) == entry.Hash {
									logger.Info("keeping local changes", "path", targetPath)
									keepLocal = true
								}
							}

							if !keepLocal {
								if rt.IsDryRun() {
									if err := logDryRunPullAction(logger, targetPath, entry, backup); err != nil {
										return err
									}
								} else if !merge {
									if err := warnIfOverwritingLocalChanges(logger, targetPath, entry, backup); err != nil {
										return err
									}
								}

								pathsToFetch = append(pathsToFetch, targetPath)
							}
						}
					}
				}

				if time.Now().Before(nextPlanProgress) {
					continue
				}
				logger.Info(
					"pull in progress",
					"stage", "plan",
					"scanned_entries", scannedEntries,
					"total_entries", len(snapshot.Entries),
					"to_fetch", len(pathsToFetch),
				)
				nextPlanProgress = time.Now().Add(5 * time.Second)
			}

			if len(pathsToFetch) == 0 {
				if deleteUnknown {
					if rt.IsDryRun() {
						deleted, err := logDryRunDeleteUnknown(logger, snapshot.Entries, backup)
						if err != nil {
							return err
						}
						if deleted > 0 {
							logger.Info("would delete unknown local files", "count", deleted)
						}
					} else {
						deleted, err := deleteUnknownLocalFiles(logger, snapshot.Entries, backup)
						if err != nil {
							return err
						}
						if deleted > 0 {
							logger.Info("deleted unknown local files", "count", deleted)
						}
					}
				}

				logLocalMatchSummary(logger, alreadyUpToDate, metadataUpdated, basesCopied)
				logger.Info("no remote files to fetch")
				return nil
			}

			if rt.IsDryRun() {
				if deleteUnknown {
					deleted, err := logDryRunDeleteUnknown(logger, snapshot.Entries, backup)
					if err != nil {
						return err
					}
					if deleted > 0 {
						logger.Info("would delete unknown local files", "count", deleted)
					}
				}
				logLocalMatchSummary(logger, alreadyUpToDate, metadataUpdated, basesCopied)
				return nil
			}

			var pulledFiles atomic.Int64
			stopDownloadProgress := startPullProgressTicker(cmd.Context(), logger, func() []any {
				return []any{
					"stage", "download",
					"pulled_files", pulledFiles.Load(),
					"total_files", len(pathsToFetch),
				}
			})
			files, refreshed, err := remotelist.ReadFilesWithProgress(
				cmd.Context(),
				logger,
				userState.Token,
				vaultState,
				pathsToFetch,
				&snapshot,
				true,
				func(_ string, pulled int, _ int) {
					pulledFiles.Store(int64(pulled))
				},
			)
			stopDownloadProgress()
			if err != nil {
				return err
			}

			if err := maybeSaveRemoteCache(cacheStore, &snapshot, refreshed, effectiveNoCache(*noCache, rt.IsDryRun())); err != nil {
				return err
			}

			for _, file := range files {
				var status localFileWriteStatus
				var baseCopied bool
				if merge {
					status, baseCopied, err = writeMergedFile(logger, file.Entry.Path, file, backup)
				} else {
					status, err = writePulledFile(logger, file.Entry.Path, file, backup)
				}
				if err != nil {
					return err
				}
				if baseCopied {
					basesCopied++
				}

				switch status {
				case localFileUnchanged:
					alreadyUpToDate++
					logger.Debug("file already up to date", "path", file.Entry.Path)
				case localFileMetadataOnly:
					metadataUpdated++
					logger.Debug("updated file metadata", "path", file.Entry.Path)
				case localFileKeptLocal:
					logger.Info("keeping local changes", "path", file.Entry.Path)
				case localFileMerged:
					logger.Info("merged file", "path", file.Entry.Path)
				case localFileConflict:
					logger.Warn("merged file contains conflicts", "path", file.Entry.Path)
				default:
					logger.Info("pulled file", "path", file.Entry.Path, "bytes", len(file.Body))
				}
			}

			if deleteUnknown {
				deleted, err := deleteUnknownLocalFiles(logger, snapshot.Entries, backup)
				if err != nil {
					return err
				}
				if deleted > 0 {
					logger.Info("deleted unknown local files", "count", deleted)
				}
			}

			logLocalMatchSummary(logger, alreadyUpToDate, metadataUpdated, basesCopied)

			return nil
		},
	}

	cmd.Flags().BoolVar(&merge, "merge", false, "three-way merge remote changes with local changes when possible")
	cmd.Flags().BoolVar(&noBackup, "no-backup", false, "do not back up overwritten or deleted local files during pull")
	cmd.Flags().BoolVar(&deleteUnknown, "delete-unknown", false, "delete non-hidden local files that do not exist in the vault")
	cmd.Flags().BoolVar(&onlyNotes, "only-notes", false, "only fetch markdown notes (*.md)")

	return cmd
}

func startPullProgressTicker(ctx context.Context, logger *slog.Logger, attrs func() []any) func() {
	done := make(chan struct{})
	var once sync.Once

	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-ticker.C:
				logger.Info("pull in progress", attrs()...)
			}
		}
	}()

	return func() {
		once.Do(func() {
			close(done)
		})
	}
}
