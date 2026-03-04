package commands

import (
	"errors"
	"os"

	"github.com/spf13/cobra"

	"github.com/wojas/ob1/internal/remotelist"
	"github.com/wojas/ob1/internal/vaultstore"
)

func NewListCommand(rt Runtime, debug *bool, noCache *bool) *cobra.Command {
	var cachedOnly bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List remote vault entries through the sync websocket",
		RunE: func(cmd *cobra.Command, _ []string) error {
			logger := rt.NewLogger(*debug)

			cacheStore := remotelist.NewCacheStore(".")
			if cachedOnly {
				if *noCache {
					return errors.New("cannot combine --cached with --no-cache")
				}

				cached, err := cacheStore.Load()
				if err != nil {
					if errors.Is(err, os.ErrNotExist) {
						return errors.New("no local cache found")
					}
					return err
				}

				return writeRemoteEntryTable(os.Stdout, cached.Entries)
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

			return writeRemoteEntryTable(os.Stdout, snapshot.Entries)
		},
	}

	cmd.Flags().BoolVar(&cachedOnly, "cached", false, "only return cached results without contacting the server")

	return cmd
}
