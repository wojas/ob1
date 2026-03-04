package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/wojas/ob1/internal/logutil"
	"github.com/wojas/ob1/internal/obsidianapi"
	"github.com/wojas/ob1/internal/remotelist"
	"github.com/wojas/ob1/internal/userstore"
	"github.com/wojas/ob1/internal/vaultcrypto"
	"github.com/wojas/ob1/internal/vaultstore"
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
	root.AddCommand(newCatCommand(app, &debug))
	root.AddCommand(newInfoCommand(app, &apiBase, &debug))
	root.AddCommand(newListRemoteCommand(app, &debug))
	root.AddCommand(newLogoutCommand(app, &apiBase, &debug))
	root.AddCommand(newVaultCommand(app, &apiBase, &debug))

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

func newCatCommand(app *app, debug *bool) *cobra.Command {
	return &cobra.Command{
		Use:   "cat <filename>",
		Short: "Write a remote file's contents to stdout",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app.logger = newLogger(*debug)

			userState, err := app.store.Load()
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

			body, err := remotelist.ReadFile(cmd.Context(), app.logger, userState.Token, vaultState, args[0])
			if err != nil {
				return err
			}

			if _, err := os.Stdout.Write(body); err != nil {
				return fmt.Errorf("write file to stdout: %w", err)
			}

			return nil
		},
	}
}

func newListRemoteCommand(app *app, debug *bool) *cobra.Command {
	return &cobra.Command{
		Use:   "list-remote",
		Short: "List remote vault entries through the sync websocket",
		RunE: func(cmd *cobra.Command, _ []string) error {
			app.logger = newLogger(*debug)

			userState, err := app.store.Load()
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

			entries, err := remotelist.Snapshot(cmd.Context(), app.logger, userState.Token, vaultState)
			if err != nil {
				return err
			}

			return writeRemoteEntryTable(os.Stdout, entries)
		},
	}
}

func newVaultCommand(app *app, apiBase *string, debug *bool) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "vault",
		Short: "Work with remote vault metadata",
	}

	cmd.AddCommand(newVaultListCommand(app, apiBase, debug))
	cmd.AddCommand(newVaultSetupCommand(app, apiBase, debug))

	return cmd
}

func newVaultListCommand(app *app, apiBase *string, debug *bool) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List remote vaults in a table",
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
			vaults, err := client.ListVaults(cmd.Context(), state.Token)
			if err != nil {
				return err
			}

			return writeVaultTable(os.Stdout, vaults)
		},
	}
}

func newVaultSetupCommand(app *app, apiBase *string, debug *bool) *cobra.Command {
	var vaultPassword string
	var deviceName string

	cmd := &cobra.Command{
		Use:   "setup <id>",
		Short: "Validate a vault password and write .ob1/vault.json",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app.logger = newLogger(*debug)

			if err := ensureCurrentDirectoryEmpty(); err != nil {
				return err
			}

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
			vaults, err := client.ListVaults(cmd.Context(), state.Token)
			if err != nil {
				return err
			}

			vault, err := findVaultByID(vaults, args[0])
			if err != nil {
				return err
			}

			if strings.TrimSpace(vault.Salt) == "" {
				return errors.New("vault does not expose a salt; managed-encryption vault setup is not implemented")
			}

			vaultPassword, err = readPasswordIfEmpty("Vault password: ", vaultPassword)
			if err != nil {
				return err
			}

			if strings.TrimSpace(deviceName) == "" {
				deviceName, err = defaultDeviceName()
				if err != nil {
					return err
				}
			}

			rawKey, err := vaultcrypto.DeriveKey(vaultPassword, vault.Salt)
			if err != nil {
				return err
			}

			keyHash, err := vaultcrypto.KeyHash(rawKey, vault.Salt, vault.EncryptionVersion)
			if err != nil {
				return err
			}

			if err := client.AccessVault(cmd.Context(), state.Token, vault, keyHash); err != nil {
				return err
			}

			info, err := client.UserInfo(cmd.Context(), state.Token)
			if err != nil {
				return err
			}

			store := vaultstore.NewInDir(".")
			if err := store.Save(vaultstore.VaultState{
				VaultID:           vault.ID,
				VaultName:         vault.Name,
				Host:              vault.Host,
				Region:            vault.Region,
				EncryptionVersion: vault.EncryptionVersion,
				EncryptionKey:     vaultcrypto.EncodeKey(rawKey),
				Salt:              vault.Salt,
				KeyHash:           keyHash,
				ConflictStrategy:  "manual",
				DeviceName:        deviceName,
				UserEmail:         info.Email,
				SyncVersion:       0,
				NeedsInitialSync:  true,
				APIBaseURL:        client.BaseURL(),
				ConfiguredAt:      time.Now().UTC(),
			}); err != nil {
				return err
			}

			app.logger.Info("vault configured", "path", store.Path(), "vault_id", vault.ID, "host", vault.Host)

			return nil
		},
	}

	cmd.Flags().StringVar(&vaultPassword, "vault-password", "", "Vault encryption password")
	cmd.Flags().StringVar(&deviceName, "device-name", "", "Device name to store in the local vault config")

	return cmd
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

func writeVaultTable(out *os.File, list obsidianapi.VaultList) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "TYPE\tNAME\tID\tREGION\tHOST\tENC"); err != nil {
		return fmt.Errorf("write vault table header: %w", err)
	}

	for _, vault := range list.Vaults {
		if err := writeVaultRow(w, "owned", vault); err != nil {
			return err
		}
	}
	for _, vault := range list.Shared {
		if err := writeVaultRow(w, "shared", vault); err != nil {
			return err
		}
	}

	if len(list.Vaults) == 0 && len(list.Shared) == 0 {
		if _, err := fmt.Fprintln(w, "(none)\t\t\t\t\t"); err != nil {
			return fmt.Errorf("write empty vault table: %w", err)
		}
	}

	if err := w.Flush(); err != nil {
		return fmt.Errorf("flush vault table: %w", err)
	}

	return nil
}

func writeVaultRow(w *tabwriter.Writer, scope string, vault obsidianapi.Vault) error {
	if _, err := fmt.Fprintf(
		w,
		"%s\t%s\t%s\t%s\t%s\t%d\n",
		scope,
		displayOrDash(vault.Name),
		displayOrDash(vault.ID),
		displayOrDash(vault.Region),
		displayOrDash(vault.Host),
		vault.EncryptionVersion,
	); err != nil {
		return fmt.Errorf("write vault row: %w", err)
	}

	return nil
}

func writeRemoteEntryTable(out *os.File, entries []remotelist.Entry) error {
	sorted := append([]remotelist.Entry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Path == sorted[j].Path {
			return sorted[i].UID < sorted[j].UID
		}
		return sorted[i].Path < sorted[j].Path
	})

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "PATH\tTYPE\tSIZE\tUID\tMTIME\tDEVICE"); err != nil {
		return fmt.Errorf("write remote table header: %w", err)
	}

	for _, entry := range sorted {
		size := "-"
		entryType := "folder"
		if !entry.Folder {
			entryType = "file"
			size = fmt.Sprintf("%d", entry.Size)
		}

		if _, err := fmt.Fprintf(
			w,
			"%s\t%s\t%s\t%d\t%s\t%s\n",
			displayOrDash(entry.Path),
			entryType,
			size,
			entry.UID,
			formatUnixMillis(entry.MTime),
			displayOrDash(entry.Device),
		); err != nil {
			return fmt.Errorf("write remote table row: %w", err)
		}
	}

	if len(sorted) == 0 {
		if _, err := fmt.Fprintln(w, "(none)\t\t\t\t\t"); err != nil {
			return fmt.Errorf("write empty remote table: %w", err)
		}
	}

	if err := w.Flush(); err != nil {
		return fmt.Errorf("flush remote table: %w", err)
	}

	return nil
}

func displayOrDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}

	return value
}

func ensureCurrentDirectoryEmpty() error {
	entries, err := os.ReadDir(".")
	if err != nil {
		return fmt.Errorf("read current directory: %w", err)
	}

	if len(entries) != 0 {
		return errors.New("current directory is not empty")
	}

	return nil
}

func findVaultByID(list obsidianapi.VaultList, id string) (obsidianapi.Vault, error) {
	for _, vault := range list.Vaults {
		if vault.ID == id {
			return vault, nil
		}
	}
	for _, vault := range list.Shared {
		if vault.ID == id {
			return vault, nil
		}
	}

	return obsidianapi.Vault{}, fmt.Errorf("vault %q not found", id)
}

func defaultDeviceName() (string, error) {
	name, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("resolve hostname: %w", err)
	}

	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("hostname is empty; pass --device-name")
	}

	return filepath.Base(name), nil
}

func formatUnixMillis(ms int64) string {
	if ms <= 0 {
		return "-"
	}

	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}
