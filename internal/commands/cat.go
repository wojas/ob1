package commands

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/wojas/ob1/internal/remotelist"
	"github.com/wojas/ob1/internal/vaultstore"
)

func NewCatCommand(rt Runtime, debug *bool, noCache *bool) *cobra.Command {
	return &cobra.Command{
		Use:   "cat <filename>",
		Short: "Write a remote file's contents to stdout",
		Args:  cobra.ExactArgs(1),
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

			body, snapshot, err := remotelist.ReadFile(cmd.Context(), logger, userState.Token, vaultState, args[0], cached, !*noCache)
			if err != nil {
				return err
			}

			if err := maybeSaveRemoteCache(cacheStore, cached, snapshot, effectiveNoCache(*noCache, rt.IsDryRun())); err != nil {
				return err
			}

			if _, err := os.Stdout.Write(body); err != nil {
				return fmt.Errorf("write file to stdout: %w", err)
			}

			return nil
		},
	}
}
