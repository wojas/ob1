package commands

import (
	"errors"
	"os"

	"github.com/spf13/cobra"

	"github.com/wojas/ob1/internal/obsidianapi"
)

func NewInfoCommand(rt Runtime, apiBase *string, debug *bool) *cobra.Command {
	return &cobra.Command{
		Use:   "info",
		Short: "Fetch basic user info for the stored session",
		RunE: func(cmd *cobra.Command, _ []string) error {
			logger := rt.NewLogger(*debug)

			state, err := rt.Store.Load()
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return errors.New("no local session found; login first")
				}

				return err
			}

			client := obsidianapi.New(currentAPIBase(state, *apiBase), logger)
			info, err := client.UserInfo(cmd.Context(), state.Token)
			if err != nil {
				return err
			}

			return writeJSON(os.Stdout, info.Raw)
		},
	}
}
