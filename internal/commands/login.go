package commands

import (
	"time"

	"github.com/spf13/cobra"

	"github.com/wojas/ob1/internal/obsidianapi"
	"github.com/wojas/ob1/internal/userstore"
)

func NewLoginCommand(rt Runtime, apiBase *string, debug *bool) *cobra.Command {
	var email string
	var password string
	var mfa string

	cmd := &cobra.Command{
		Use:   "login",
		Short: "Sign in and persist the auth token locally",
		RunE: func(cmd *cobra.Command, _ []string) error {
			logger := rt.NewLogger(*debug)

			var err error
			email, err = readLineIfEmpty("Email: ", email)
			if err != nil {
				return err
			}

			password, err = readPasswordIfEmpty("Password: ", password, "--password")
			if err != nil {
				return err
			}

			client := obsidianapi.New(*apiBase, logger)
			session, err := client.SignIn(cmd.Context(), obsidianapi.SignInRequest{
				Email:    email,
				Password: password,
				MFA:      mfa,
			})
			if err != nil {
				return err
			}

			if err := rt.Store.Save(userstore.UserState{
				APIBaseURL: client.BaseURL(),
				Token:      session.Token,
				User:       session.User,
				SavedAt:    time.Now().UTC(),
			}); err != nil {
				return err
			}

			logger.Info("login succeeded", "path", rt.Store.Path())

			return nil
		},
	}

	cmd.Flags().StringVar(&email, "email", "", "Obsidian account email")
	cmd.Flags().StringVar(&password, "password", "", "Obsidian account password")
	cmd.Flags().StringVar(&mfa, "mfa", "", "MFA code when required")

	return cmd
}
