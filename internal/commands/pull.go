package commands

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wojas/ob1/internal/remotelist"
	"github.com/wojas/ob1/internal/vaultstore"
)

func NewPullCommand(rt Runtime, debug *bool, noCache *bool) *cobra.Command {
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

			snapshot, err := remotelist.SyncEntries(cmd.Context(), logger, userState.Token, vaultState, cached, !*noCache)
			if err != nil {
				return err
			}

			if err := maybeSaveRemoteCache(cacheStore, cached, snapshot, effectiveNoCache(*noCache, rt.IsDryRun())); err != nil {
				return err
			}

			pathsToFetch := make([]string, 0, len(snapshot.Entries))
			alreadyUpToDate := 0
			metadataUpdated := 0
			for _, entry := range snapshot.Entries {
				if entry.Folder {
					continue
				}
				if onlyNotes && !strings.EqualFold(filepath.Ext(entry.Path), ".md") {
					continue
				}

				targetPath, ok := safeLocalTarget(entry.Path)
				if !ok {
					logger.Warn("skipping dangerous path", "path", entry.Path)
					continue
				}

				upToDate, metadataOnly, err := localFileMatchesRemote(targetPath, entry, !rt.IsDryRun())
				if err != nil {
					return err
				}
				if upToDate {
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
					continue
				}

				if rt.IsDryRun() {
					if err := logDryRunPullAction(logger, targetPath, entry, backup); err != nil {
						return err
					}
				} else if err := warnIfOverwritingLocalChanges(logger, targetPath, entry, backup); err != nil {
					return err
				}

				pathsToFetch = append(pathsToFetch, targetPath)
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

				logLocalMatchSummary(logger, alreadyUpToDate, metadataUpdated)
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
				logLocalMatchSummary(logger, alreadyUpToDate, metadataUpdated)
				return nil
			}

			files, refreshed, err := remotelist.ReadFiles(cmd.Context(), logger, userState.Token, vaultState, pathsToFetch, &snapshot, true)
			if err != nil {
				return err
			}

			if err := maybeSaveRemoteCache(cacheStore, &snapshot, refreshed, effectiveNoCache(*noCache, rt.IsDryRun())); err != nil {
				return err
			}

			for _, file := range files {
				status, err := writePulledFile(logger, file.Entry.Path, file, backup)
				if err != nil {
					return err
				}

				switch status {
				case localFileUnchanged:
					alreadyUpToDate++
					logger.Debug("file already up to date", "path", file.Entry.Path)
				case localFileMetadataOnly:
					metadataUpdated++
					logger.Debug("updated file metadata", "path", file.Entry.Path)
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

			logLocalMatchSummary(logger, alreadyUpToDate, metadataUpdated)

			return nil
		},
	}

	cmd.Flags().BoolVar(&noBackup, "no-backup", false, "do not back up overwritten or deleted local files during pull")
	cmd.Flags().BoolVar(&deleteUnknown, "delete-unknown", false, "delete non-hidden local files that do not exist in the vault")
	cmd.Flags().BoolVar(&onlyNotes, "only-notes", false, "only fetch markdown notes (*.md)")

	return cmd
}
