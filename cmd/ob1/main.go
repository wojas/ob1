package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/wojas/ob1/internal/logutil"
	"github.com/wojas/ob1/internal/obsidianapi"
	"github.com/wojas/ob1/internal/userstore"
)

type app struct {
	logger *slog.Logger
	store  *userstore.Store
}

func main() {
	os.Exit(run())
}

func run() int {
	store, err := userstore.NewDefault()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	var debug bool
	var apiBase string

	root := &cobra.Command{
		Use:           "ob1",
		Short:         "Alternative Obsidian headless client",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.PersistentFlags().BoolVarP(&debug, "debug", "v", false, "enable debug logging")
	root.PersistentFlags().StringVar(&apiBase, "api-base", defaultAPIBase(), "Obsidian API base URL")

	app := &app{
		logger: newLogger(debug),
		store:  store,
	}

	root.AddCommand(newLoginCommand(app, &apiBase, &debug))
	root.AddCommand(newInfoCommand(app, &apiBase, &debug))
	root.AddCommand(newLogoutCommand(app, &apiBase, &debug))

	if err := root.ExecuteContext(context.Background()); err != nil {
		app.logger.Error(err.Error())
		return 1
	}

	return 0
}

func newLoginCommand(app *app, apiBase *string, debug *bool) *cobra.Command {
	var email string
	var password string
	var mfa string

	cmd := &cobra.Command{
		Use:   "login",
		Short: "Sign in and persist the auth token locally",
		RunE: func(cmd *cobra.Command, _ []string) error {
			app.logger = newLogger(*debug)

			var err error
			email, err = readLineIfEmpty("Email: ", email)
			if err != nil {
				return err
			}

			password, err = readPasswordIfEmpty("Password: ", password)
			if err != nil {
				return err
			}

			client := obsidianapi.New(*apiBase, app.logger)
			session, err := client.SignIn(cmd.Context(), obsidianapi.SignInRequest{
				Email:    email,
				Password: password,
				MFA:      mfa,
			})
			if err != nil {
				return err
			}

			if err := app.store.Save(userstore.UserState{
				APIBaseURL: client.BaseURL(),
				Token:      session.Token,
				User:       session.User,
				SavedAt:    time.Now().UTC(),
			}); err != nil {
				return err
			}

			app.logger.Info("login succeeded", "path", app.store.Path())

			return nil
		},
	}

	cmd.Flags().StringVar(&email, "email", "", "Obsidian account email")
	cmd.Flags().StringVar(&password, "password", "", "Obsidian account password")
	cmd.Flags().StringVar(&mfa, "mfa", "", "MFA code when required")

	return cmd
}

func newLogoutCommand(app *app, apiBase *string, debug *bool) *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Sign out remotely and remove the local auth token",
		RunE: func(cmd *cobra.Command, _ []string) error {
			app.logger = newLogger(*debug)

			state, err := app.store.Load()
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					app.logger.Info("no local session found", "path", app.store.Path())
					return nil
				}

				return err
			}

			baseURL := strings.TrimSpace(state.APIBaseURL)
			if baseURL == "" {
				baseURL = *apiBase
			}

			client := obsidianapi.New(baseURL, app.logger)
			remoteErr := client.SignOut(cmd.Context(), state.Token)
			if remoteErr != nil {
				app.logger.Warn("remote signout failed", "err", remoteErr)
			}

			if err := app.store.Delete(); err != nil {
				return err
			}

			app.logger.Info("local session removed", "path", app.store.Path())

			return remoteErr
		},
	}
}

func newInfoCommand(app *app, apiBase *string, debug *bool) *cobra.Command {
	return &cobra.Command{
		Use:   "info",
		Short: "Fetch basic user info for the stored session",
		RunE: func(cmd *cobra.Command, _ []string) error {
			app.logger = newLogger(*debug)

			state, err := app.store.Load()
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return errors.New("no local session found; login first")
				}

				return err
			}

			baseURL := strings.TrimSpace(state.APIBaseURL)
			if baseURL == "" {
				baseURL = *apiBase
			}

			client := obsidianapi.New(baseURL, app.logger)
			info, err := client.UserInfo(cmd.Context(), state.Token)
			if err != nil {
				return err
			}

			return writeJSON(os.Stdout, info.Raw)
		},
	}
}

func newLogger(debug bool) *slog.Logger {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}

	return slog.New(logutil.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: level,
	}, term.IsTerminal(int(os.Stderr.Fd()))))
}

func defaultAPIBase() string {
	if value := strings.TrimSpace(os.Getenv("OB1_API_BASE")); value != "" {
		return value
	}

	return obsidianapi.DefaultBaseURL
}

func readLineIfEmpty(prompt string, current string) (string, error) {
	if strings.TrimSpace(current) != "" {
		return current, nil
	}

	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", errors.New("missing required value; pass the flag explicitly when stdin is not a terminal")
	}

	fmt.Fprint(os.Stderr, prompt)
	reader := bufio.NewReader(os.Stdin)
	value, err := reader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("read input: %w", err)
	}

	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("value cannot be empty")
	}

	return value, nil
}

func readPasswordIfEmpty(prompt string, current string) (string, error) {
	if current != "" {
		return current, nil
	}

	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", errors.New("missing password; pass --password when stdin is not a terminal")
	}

	fmt.Fprint(os.Stderr, prompt)
	password, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}

	if len(password) == 0 {
		return "", errors.New("password cannot be empty")
	}

	return string(password), nil
}

func writeJSON(out *os.File, body []byte) error {
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return fmt.Errorf("decode response JSON: %w", err)
	}

	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode response JSON: %w", err)
	}

	if _, err := out.Write(append(encoded, '\n')); err != nil {
		return fmt.Errorf("write response JSON: %w", err)
	}

	return nil
}
