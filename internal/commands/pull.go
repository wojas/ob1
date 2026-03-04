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

	cmd := &cobra.Command{
		Use:   "pull",
		Short: "Fetch all remote files into the current directory",
		RunE: func(cmd *cobra.Command, _ []string) error {
			logger := rt.NewLogger(*debug)

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

			if err := maybeSaveRemoteCache(cacheStore, cached, snapshot, *noCache); err != nil {
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

				upToDate, metadataOnly, err := localFileMatchesRemote(targetPath, entry)
				if err != nil {
					return err
				}
				if upToDate {
					if metadataOnly {
						metadataUpdated++
						logger.Debug("updated file metadata", "path", targetPath)
					} else {
						alreadyUpToDate++
						logger.Debug("file already up to date", "path", targetPath)
					}
					continue
				}

				if err := warnIfOverwritingLocalChanges(logger, targetPath, entry); err != nil {
					return err
				}

				pathsToFetch = append(pathsToFetch, targetPath)
			}

			if len(pathsToFetch) == 0 {
				logLocalMatchSummary(logger, alreadyUpToDate, metadataUpdated)
				logger.Info("no remote files to fetch")
				return nil
			}

			files, refreshed, err := remotelist.ReadFiles(cmd.Context(), logger, userState.Token, vaultState, pathsToFetch, &snapshot, true)
			if err != nil {
				return err
			}

			if err := maybeSaveRemoteCache(cacheStore, &snapshot, refreshed, *noCache); err != nil {
				return err
			}

			for _, file := range files {
				if err := writeLocalFileForce(file.Entry.Path, file); err != nil {
					return err
				}

				logger.Info("pulled file", "path", file.Entry.Path, "bytes", len(file.Body))
			}

			logLocalMatchSummary(logger, alreadyUpToDate, metadataUpdated)

			return nil
		},
	}

	cmd.Flags().BoolVar(&onlyNotes, "only-notes", false, "only fetch markdown notes (*.md)")

	return cmd
}
