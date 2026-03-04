package commands

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/wojas/ob1/internal/remotelist"
	"github.com/wojas/ob1/internal/vaultstore"
)

func NewGetCommand(rt Runtime, debug *bool, noCache *bool) *cobra.Command {
	return &cobra.Command{
		Use:   "get <file1> [file2] [fileN]",
		Short: "Fetch remote files into the current directory",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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

			entriesByPath := make(map[string]remotelist.Entry, len(snapshot.Entries))
			for _, entry := range snapshot.Entries {
				entriesByPath[entry.Path] = entry
			}

			pathsToFetch := make([]string, 0, len(args))
			alreadyUpToDate := 0
			metadataUpdated := 0
			for _, arg := range args {
				targetPath, ok := safeLocalTarget(arg)
				if !ok {
					logger.Warn("skipping dangerous path", "path", arg)
					continue
				}

				entry, ok := entriesByPath[targetPath]
				if !ok {
					return fmt.Errorf("remote file %q not found", targetPath)
				}
				if entry.Folder {
					return fmt.Errorf("%q is a folder", targetPath)
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

				pathsToFetch = append(pathsToFetch, targetPath)
			}

			if len(pathsToFetch) == 0 {
				logLocalMatchSummary(logger, alreadyUpToDate, metadataUpdated)
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
				status, err := writeLocalFile(file.Entry.Path, file)
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
					logger.Info("fetched file", "path", file.Entry.Path, "bytes", len(file.Body))
				}
			}

			logLocalMatchSummary(logger, alreadyUpToDate, metadataUpdated)

			return nil
		},
	}
}
