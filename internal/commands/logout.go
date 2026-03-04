package commands

import (
	"errors"
	"os"

	"github.com/spf13/cobra"

	"github.com/wojas/ob1/internal/obsidianapi"
)

func NewLogoutCommand(rt Runtime, apiBase *string, debug *bool) *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Sign out remotely and remove the local auth token",
		RunE: func(cmd *cobra.Command, _ []string) error {
			logger := rt.NewLogger(*debug)

			state, err := rt.Store.Load()
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					logger.Info("no local session found", "path", rt.Store.Path())
					return nil
				}

				return err
			}

			client := obsidianapi.New(currentAPIBase(state, *apiBase), logger)
			remoteErr := client.SignOut(cmd.Context(), state.Token)
			if remoteErr != nil {
				logger.Warn("remote signout failed", "err", remoteErr)
			}

			if err := rt.Store.Delete(); err != nil {
				return err
			}

			logger.Info("local session removed", "path", rt.Store.Path())

			return remoteErr
		},
	}
}
