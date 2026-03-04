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
	var noCache bool
	var apiBase string

	root := &cobra.Command{
		Use:           "ob1",
		Short:         "Alternative Obsidian headless client",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.PersistentFlags().BoolVarP(&debug, "debug", "v", false, "enable debug logging")
	root.PersistentFlags().BoolVar(&noCache, "no-cache", false, "skip reading and writing the local remote snapshot cache")
	root.PersistentFlags().StringVar(&apiBase, "api-base", defaultAPIBase(), "Obsidian API base URL")

	app := &app{
		logger: newLogger(debug),
		store:  store,
	}

	root.AddCommand(newLoginCommand(app, &apiBase, &debug))
	root.AddCommand(newCatCommand(app, &debug, &noCache))
	root.AddCommand(newGetCommand(app, &debug, &noCache))
	root.AddCommand(newInfoCommand(app, &apiBase, &debug))
	root.AddCommand(newListRemoteCommand(app, &debug, &noCache))
	root.AddCommand(newPullCommand(app, &debug, &noCache))
	root.AddCommand(newPutCommand(app, &debug, &noCache))
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

func newCatCommand(app *app, debug *bool, noCache *bool) *cobra.Command {
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

			cacheStore := remotelist.NewCacheStore(".")
			cached, cacheErr := loadRemoteCache(cacheStore, *noCache)
			if cacheErr != nil {
				return cacheErr
			}

			body, snapshot, err := remotelist.ReadFile(cmd.Context(), app.logger, userState.Token, vaultState, args[0], cached, !*noCache)
			if err != nil {
				return err
			}

			if err := maybeSaveRemoteCache(cacheStore, cached, snapshot, *noCache); err != nil {
				return err
			}

			if _, err := os.Stdout.Write(body); err != nil {
				return fmt.Errorf("write file to stdout: %w", err)
			}

			return nil
		},
	}
}

func newGetCommand(app *app, debug *bool, noCache *bool) *cobra.Command {
	return &cobra.Command{
		Use:   "get <file1> [file2] [fileN]",
		Short: "Fetch remote files into the current directory",
		Args:  cobra.MinimumNArgs(1),
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

			cacheStore := remotelist.NewCacheStore(".")
			cached, cacheErr := loadRemoteCache(cacheStore, *noCache)
			if cacheErr != nil {
				return cacheErr
			}

			snapshot, err := remotelist.SyncEntries(cmd.Context(), app.logger, userState.Token, vaultState, cached, !*noCache)
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
					app.logger.Warn("skipping dangerous path", "path", arg)
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
						app.logger.Debug("updated file metadata", "path", targetPath)
					} else {
						alreadyUpToDate++
						app.logger.Debug("file already up to date", "path", targetPath)
					}
					continue
				}

				pathsToFetch = append(pathsToFetch, targetPath)
			}

			if len(pathsToFetch) == 0 {
				logLocalMatchSummary(app.logger, alreadyUpToDate, metadataUpdated)
				return nil
			}

			files, refreshed, err := remotelist.ReadFiles(cmd.Context(), app.logger, userState.Token, vaultState, pathsToFetch, &snapshot, true)
			if err != nil {
				return err
			}

			if err := maybeSaveRemoteCache(cacheStore, &snapshot, refreshed, *noCache); err != nil {
				return err
			}

			for _, file := range files {
				targetPath := file.Entry.Path
				status, err := writeLocalFile(targetPath, file)
				if err != nil {
					return err
				}

				switch status {
				case localFileUnchanged:
					alreadyUpToDate++
					app.logger.Debug("file already up to date", "path", targetPath)
				case localFileMetadataOnly:
					metadataUpdated++
					app.logger.Debug("updated file metadata", "path", targetPath)
				default:
					app.logger.Info("fetched file", "path", targetPath, "bytes", len(file.Body))
				}
			}

			logLocalMatchSummary(app.logger, alreadyUpToDate, metadataUpdated)

			return nil
		},
	}
}

func newPullCommand(app *app, debug *bool, noCache *bool) *cobra.Command {
	var onlyNotes bool

	cmd := &cobra.Command{
		Use:   "pull",
		Short: "Fetch all remote files into the current directory",
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

			cacheStore := remotelist.NewCacheStore(".")
			cached, cacheErr := loadRemoteCache(cacheStore, *noCache)
			if cacheErr != nil {
				return cacheErr
			}

			snapshot, err := remotelist.SyncEntries(cmd.Context(), app.logger, userState.Token, vaultState, cached, !*noCache)
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
					app.logger.Warn("skipping dangerous path", "path", entry.Path)
					continue
				}

				upToDate, metadataOnly, err := localFileMatchesRemote(targetPath, entry)
				if err != nil {
					return err
				}
				if upToDate {
					if metadataOnly {
						metadataUpdated++
						app.logger.Debug("updated file metadata", "path", targetPath)
					} else {
						alreadyUpToDate++
						app.logger.Debug("file already up to date", "path", targetPath)
					}
					continue
				}

				if err := warnIfOverwritingLocalChanges(app.logger, targetPath, entry); err != nil {
					return err
				}

				pathsToFetch = append(pathsToFetch, targetPath)
			}

			if len(pathsToFetch) == 0 {
				logLocalMatchSummary(app.logger, alreadyUpToDate, metadataUpdated)
				app.logger.Info("no remote files to fetch")
				return nil
			}

			files, refreshed, err := remotelist.ReadFiles(cmd.Context(), app.logger, userState.Token, vaultState, pathsToFetch, &snapshot, true)
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

				app.logger.Info("pulled file", "path", file.Entry.Path, "bytes", len(file.Body))
			}

			logLocalMatchSummary(app.logger, alreadyUpToDate, metadataUpdated)

			return nil
		},
	}

	cmd.Flags().BoolVar(&onlyNotes, "only-notes", false, "only fetch markdown notes (*.md)")

	return cmd
}

func newPutCommand(app *app, debug *bool, noCache *bool) *cobra.Command {
	return &cobra.Command{
		Use:   "put <file1> [file2] [...]",
		Short: "Upload local files into the vault",
		Args:  cobra.MinimumNArgs(1),
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

			uploads := make([]remotelist.Upload, 0, len(args))
			for _, arg := range args {
				remotePath, ok := safeLocalTarget(arg)
				if !ok {
					app.logger.Warn("skipping dangerous path", "path", arg)
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
				app.logger.Warn("no safe files to upload")
				return nil
			}

			cacheStore := remotelist.NewCacheStore(".")
			cached, cacheErr := loadRemoteCache(cacheStore, *noCache)
			if cacheErr != nil {
				return cacheErr
			}

			_, err = remotelist.PutFiles(cmd.Context(), app.logger, userState.Token, vaultState, uploads, cached, !*noCache)
			return err
		},
	}
}

func newListRemoteCommand(app *app, debug *bool, noCache *bool) *cobra.Command {
	var cachedOnly bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List remote vault entries through the sync websocket",
		RunE: func(cmd *cobra.Command, _ []string) error {
			app.logger = newLogger(*debug)

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

			cached, cacheErr := loadRemoteCache(cacheStore, *noCache)
			if cacheErr != nil {
				return cacheErr
			}

			snapshot, err := remotelist.SyncEntries(cmd.Context(), app.logger, userState.Token, vaultState, cached, !*noCache)
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
	if _, err := fmt.Fprintln(w, "PATH\tTYPE\tSIZE\tUID\tCTIME\tMTIME\tDEVICE"); err != nil {
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
			"%s\t%s\t%s\t%d\t%s\t%s\t%s\n",
			displayOrDash(entry.Path),
			entryType,
			size,
			entry.UID,
			formatUnixMillis(entry.CTime),
			formatUnixMillis(entry.MTime),
			displayOrDash(entry.Device),
		); err != nil {
			return fmt.Errorf("write remote table row: %w", err)
		}
	}

	if len(sorted) == 0 {
		if _, err := fmt.Fprintln(w, "(none)\t\t\t\t\t\t"); err != nil {
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

func safeLocalTarget(remotePath string) (string, bool) {
	trimmed := strings.TrimSpace(remotePath)
	if trimmed == "" {
		return "", false
	}

	cleaned := filepath.Clean(trimmed)
	if cleaned == "." {
		return "", false
	}
	if filepath.IsAbs(cleaned) {
		return "", false
	}
	if cleaned == ".." {
		return "", false
	}

	parentPrefix := ".." + string(filepath.Separator)
	if strings.HasPrefix(cleaned, parentPrefix) {
		return "", false
	}

	if vol := filepath.VolumeName(cleaned); vol != "" {
		return "", false
	}

	return cleaned, true
}

func localFileMatchesRemote(path string, entry remotelist.Entry) (bool, bool, error) {
	remoteHash := strings.TrimSpace(entry.Hash)
	if remoteHash == "" {
		return false, false, nil
	}

	body, err := os.ReadFile(path)
	switch {
	case err == nil:
	case errors.Is(err, os.ErrNotExist):
		return false, false, nil
	default:
		return false, false, fmt.Errorf("read %s: %w", path, err)
	}

	if vaultcrypto.PlaintextHash(body) != remoteHash {
		return false, false, nil
	}

	updated, err := updateLocalFileTimes(path, entry.MTime)
	if err != nil {
		return false, false, err
	}

	return true, updated, nil
}

type localFileWriteStatus int

const (
	localFileUnchanged localFileWriteStatus = iota
	localFileMetadataOnly
	localFileContentUpdated
)

func writeLocalFile(path string, file remotelist.File) (localFileWriteStatus, error) {
	dir := filepath.Dir(path)
	if dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return localFileUnchanged, fmt.Errorf("create directories for %s: %w", path, err)
		}
	}

	remoteHash := strings.TrimSpace(file.Entry.Hash)
	if remoteHash != "" {
		existingBody, err := os.ReadFile(path)
		switch {
		case err == nil:
			if vaultcrypto.PlaintextHash(existingBody) == remoteHash {
				updated, err := updateLocalFileTimes(path, file.Entry.MTime)
				if err != nil {
					return localFileUnchanged, err
				}
				if updated {
					return localFileMetadataOnly, nil
				}
				return localFileUnchanged, nil
			}
		case errors.Is(err, os.ErrNotExist):
		default:
			return localFileUnchanged, fmt.Errorf("read %s: %w", path, err)
		}
	}

	if err := os.WriteFile(path, file.Body, 0o644); err != nil {
		return localFileUnchanged, fmt.Errorf("write %s: %w", path, err)
	}

	if _, err := updateLocalFileTimes(path, file.Entry.MTime); err != nil {
		return localFileUnchanged, err
	}

	return localFileContentUpdated, nil
}

func writeLocalFileForce(path string, file remotelist.File) error {
	dir := filepath.Dir(path)
	if dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create directories for %s: %w", path, err)
		}
	}

	if err := os.WriteFile(path, file.Body, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	if _, err := updateLocalFileTimes(path, file.Entry.MTime); err != nil {
		return err
	}

	return nil
}

func warnIfOverwritingLocalChanges(logger *slog.Logger, path string, entry remotelist.Entry) error {
	remoteHash := strings.TrimSpace(entry.Hash)
	if remoteHash == "" {
		return nil
	}

	body, err := os.ReadFile(path)
	switch {
	case err == nil:
	case errors.Is(err, os.ErrNotExist):
		return nil
	default:
		return fmt.Errorf("read %s: %w", path, err)
	}

	if vaultcrypto.PlaintextHash(body) == remoteHash {
		return nil
	}

	logger.Warn("overwriting local changes", "path", path)

	return nil
}

func logLocalMatchSummary(logger *slog.Logger, alreadyUpToDate int, metadataUpdated int) {
	if alreadyUpToDate == 0 && metadataUpdated == 0 {
		return
	}

	logger.Info("local file status", "already_up_to_date", alreadyUpToDate, "metadata_updated", metadataUpdated)
}

func updateLocalFileTimes(path string, mtimeMs int64) (bool, error) {
	if mtimeMs <= 0 {
		return false, nil
	}

	info, err := os.Stat(path)
	if err != nil {
		return false, fmt.Errorf("stat %s: %w", path, err)
	}

	mtime := time.UnixMilli(mtimeMs)
	if info.ModTime().Equal(mtime) {
		return false, nil
	}

	if err := os.Chtimes(path, mtime, mtime); err != nil {
		return false, fmt.Errorf("set timestamps on %s: %w", path, err)
	}

	return true, nil
}

func loadRemoteCache(store *remotelist.CacheStore, noCache bool) (*remotelist.CacheState, error) {
	if noCache {
		return nil, nil
	}

	state, err := store.Load()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	return &state, nil
}

func maybeSaveRemoteCache(store *remotelist.CacheStore, previous *remotelist.CacheState, next remotelist.CacheState, noCache bool) error {
	if noCache {
		return nil
	}

	if previous != nil && next.Version <= previous.Version {
		return nil
	}

	if previous != nil {
		if next.SchemaVersion == 0 {
			next.SchemaVersion = previous.SchemaVersion
		}
		if next.Extra == nil && len(previous.Extra) != 0 {
			next.Extra = previous.Extra
		}
	}

	return store.Save(next)
}
