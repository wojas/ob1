package commands

import (
	"errors"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/wojas/ob1/internal/obsidianapi"
	"github.com/wojas/ob1/internal/vaultcrypto"
	"github.com/wojas/ob1/internal/vaultstore"
)

func NewVaultCommand(rt Runtime, apiBase *string, debug *bool) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "vault",
		Short: "Work with remote vault metadata",
	}

	cmd.AddCommand(newVaultListCommand(rt, apiBase, debug))
	cmd.AddCommand(newVaultSetupCommand(rt, apiBase, debug))

	return cmd
}

func newVaultListCommand(rt Runtime, apiBase *string, debug *bool) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List remote vaults in a table",
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
			vaults, err := client.ListVaults(cmd.Context(), state.Token)
			if err != nil {
				return err
			}

			return writeVaultTable(os.Stdout, vaults)
		},
	}
}

func newVaultSetupCommand(rt Runtime, apiBase *string, debug *bool) *cobra.Command {
	var vaultPassword string
	var deviceName string

	cmd := &cobra.Command{
		Use:   "setup <id>",
		Short: "Validate a vault password and write .ob1/vault.json",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			logger := rt.NewLogger(*debug)

			if err := ensureCurrentDirectoryEmpty(); err != nil {
				return err
			}

			state, err := rt.Store.Load()
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return errors.New("no local session found; login first")
				}

				return err
			}

			client := obsidianapi.New(currentAPIBase(state, *apiBase), logger)
			vaults, err := client.ListVaults(cmd.Context(), state.Token)
			if err != nil {
				return err
			}

			vault, err := findVaultByID(vaults, args[0])
			if err != nil {
				return err
			}

			if vault.Salt == "" {
				return errors.New("vault does not expose a salt; managed-encryption vault setup is not implemented")
			}

			vaultPassword, err = readPasswordIfEmpty("Vault password: ", vaultPassword, "--vault-password")
			if err != nil {
				return err
			}

			if deviceName == "" {
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

			logger.Info("vault configured", "path", store.Path(), "vault_id", vault.ID, "host", vault.Host)

			return nil
		},
	}

	cmd.Flags().StringVar(&vaultPassword, "vault-password", "", "Vault encryption password")
	cmd.Flags().StringVar(&deviceName, "device-name", "", "Device name to store in the local vault config")

	return cmd
}
