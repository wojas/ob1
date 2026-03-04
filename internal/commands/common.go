package commands

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"github.com/wojas/ob1/internal/obsidianapi"
	"github.com/wojas/ob1/internal/remotelist"
	"github.com/wojas/ob1/internal/userstore"
	"github.com/wojas/ob1/internal/vaultcrypto"
)

type Runtime struct {
	Store     *userstore.Store
	NewLogger func(debug bool) *slog.Logger
}

func currentAPIBase(state userstore.UserState, fallback string) string {
	baseURL := strings.TrimSpace(state.APIBaseURL)
	if baseURL == "" {
		baseURL = fallback
	}

	return baseURL
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

func readPasswordIfEmpty(prompt string, current string, flagName string) (string, error) {
	if current != "" {
		return current, nil
	}

	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", fmt.Errorf("missing password; pass %s when stdin is not a terminal", flagName)
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

func writeJSON(out io.Writer, body []byte) error {
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

func writeVaultTable(out io.Writer, list obsidianapi.VaultList) error {
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

func writeRemoteEntryTable(out io.Writer, entries []remotelist.Entry) error {
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

func deleteUnknownLocalFiles(logger *slog.Logger, entries []remotelist.Entry) (int, error) {
	remoteFiles := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if entry.Folder {
			continue
		}

		targetPath, ok := safeLocalTarget(entry.Path)
		if !ok {
			continue
		}

		remoteFiles[targetPath] = struct{}{}
	}

	deleted := 0
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == "." {
			return nil
		}

		name := d.Name()
		if strings.HasPrefix(name, ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			logger.Warn("skipping symlink during delete-unknown", "path", path)
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}

		relPath := filepath.Clean(path)
		if _, ok := remoteFiles[relPath]; ok {
			return nil
		}
		if err := ensureNoSymlinkAncestor(relPath); err != nil {
			return err
		}

		if err := os.Remove(relPath); err != nil {
			return fmt.Errorf("remove %s: %w", relPath, err)
		}

		deleted++
		logger.Info("deleted unknown local file", "path", relPath)

		return nil
	})
	if err != nil {
		return deleted, fmt.Errorf("scan local files for deletion: %w", err)
	}

	return deleted, nil
}

func ensureNoSymlinkAncestor(path string) error {
	dir := filepath.Dir(path)
	if dir == "." {
		return nil
	}

	current := ""
	for _, part := range strings.Split(dir, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		if current == "" {
			current = part
		} else {
			current = filepath.Join(current, part)
		}

		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("lstat %s: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to delete through symlinked directory %s", current)
		}
	}

	return nil
}
