package commands

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/wojas/ob1/internal/remotelist"
	"github.com/wojas/ob1/internal/vaultstore"
)

func NewPutCommand(rt Runtime, debug *bool, noCache *bool) *cobra.Command {
	return &cobra.Command{
		Use:   "put <file1> [file2] [...]",
		Short: "Upload local files into the vault",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := rt.requireWritable("put"); err != nil {
				return err
			}

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

			uploads := make([]remotelist.Upload, 0, len(args))
			for _, arg := range args {
				remotePath, ok := safeLocalTarget(arg)
				if !ok {
					logger.Warn("skipping dangerous path", "path", arg)
					continue
				}

				info, err := os.Stat(remotePath)
				if err != nil {
					return fmt.Errorf("stat %s: %w", remotePath, err)
				}
				if info.IsDir() {
					return fmt.Errorf("%s is a directory", remotePath)
				}

				body, err := os.ReadFile(remotePath)
				if err != nil {
					return fmt.Errorf("read %s: %w", remotePath, err)
				}

				uploads = append(uploads, remotelist.Upload{
					Path:  filepath.ToSlash(remotePath),
					Body:  body,
					MTime: info.ModTime().UnixMilli(),
				})
			}

			if len(uploads) == 0 {
				logger.Warn("no safe files to upload")
				return nil
			}

			cacheStore := remotelist.NewCacheStore(".")
			cached, err := loadRemoteCache(cacheStore, *noCache)
			if err != nil {
				return err
			}

			_, err = remotelist.PutFiles(cmd.Context(), logger, userState.Token, vaultState, uploads, cached, !*noCache)
			return err
		},
	}
}
