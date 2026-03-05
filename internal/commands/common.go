package commands

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"golang.org/x/term"

	"github.com/wojas/ob1/internal/obsidianapi"
	"github.com/wojas/ob1/internal/remotelist"
	"github.com/wojas/ob1/internal/userstore"
	"github.com/wojas/ob1/internal/vaultcrypto"
)

type Runtime struct {
	Store     *userstore.Store
	NewLogger func(debug bool) *slog.Logger
	DryRun    *bool
	Verify    *bool
}

type backupSession struct {
	root string
}

const (
	baseDirName              = ".ob1/base"
	baseDirPerm  os.FileMode = 0o700
	baseFilePerm os.FileMode = 0o600
)

func (rt Runtime) IsDryRun() bool {
	return rt.DryRun != nil && *rt.DryRun
}

func (rt Runtime) IsVerify() bool {
	return rt.Verify != nil && *rt.Verify
}

func (rt Runtime) requireWritable(command string) error {
	if !rt.IsDryRun() {
		return nil
	}

	return fmt.Errorf("--dry-run is not supported for %s", command)
}

func effectiveNoCache(noCache bool, dryRun bool) bool {
	return noCache || dryRun
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

func localFileMatchesRemote(path string, entry remotelist.Entry, applyMetadata bool, verify bool) (bool, bool, error) {
	info, err := os.Stat(path)
	switch {
	case err == nil:
	case errors.Is(err, os.ErrNotExist):
		return false, false, nil
	default:
		return false, false, fmt.Errorf("stat %s: %w", path, err)
	}

	remoteMTime := entry.MTime
	remoteSize := entry.Size
	if remoteMTime > 0 && !info.ModTime().Equal(time.UnixMilli(remoteMTime)) {
		remoteMTime = -1
	}
	if remoteSize >= 0 && info.Size() != remoteSize {
		remoteSize = -1
	}
	if remoteMTime > 0 && remoteSize >= 0 {
		remoteHash := strings.TrimSpace(entry.Hash)
		if !verify || remoteHash == "" {
			return true, false, nil
		}

		body, err := os.ReadFile(path)
		if err != nil {
			return false, false, fmt.Errorf("read %s: %w", path, err)
		}
		if vaultcrypto.PlaintextHash(body) == remoteHash {
			return true, false, nil
		}
		return false, false, nil
	}

	remoteHash := strings.TrimSpace(entry.Hash)
	if remoteHash == "" {
		return false, false, nil
	}

	body, err := os.ReadFile(path)
	if err != nil {
		return false, false, fmt.Errorf("read %s: %w", path, err)
	}

	if vaultcrypto.PlaintextHash(body) != remoteHash {
		return false, false, nil
	}

	if entry.MTime <= 0 {
		return true, false, nil
	}

	mtime := time.UnixMilli(entry.MTime)
	if info.ModTime().Equal(mtime) {
		return true, false, nil
	}

	if !applyMetadata {
		return true, true, nil
	}

	if err := os.Chtimes(path, mtime, mtime); err != nil {
		return false, false, fmt.Errorf("set timestamps on %s: %w", path, err)
	}

	return true, true, nil
}

type localFileWriteStatus int

const (
	localFileUnchanged localFileWriteStatus = iota
	localFileMetadataOnly
	localFileContentUpdated
	localFileKeptLocal
	localFileMerged
	localFileConflict
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

func warnIfOverwritingLocalChanges(logger *slog.Logger, path string, entry remotelist.Entry, backup *backupSession) error {
	remoteHash := strings.TrimSpace(entry.Hash)
	if remoteHash == "" {
		info, err := os.Lstat(path)
		switch {
		case errors.Is(err, os.ErrNotExist):
			return nil
		case err != nil:
			return fmt.Errorf("lstat %s: %w", path, err)
		}

		if backup != nil {
			logger.Warn("overwriting local file", "path", path, "backup", backup.target(path))
		} else if !info.IsDir() {
			logger.Warn("overwriting local file without backup", "path", path)
		}

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

	if backup != nil {
		logger.Warn("overwriting local file", "path", path, "backup", backup.target(path))
	} else {
		logger.Warn("overwriting local file without backup", "path", path)
	}

	return nil
}

func diff3Merge(localBody []byte, baseBody []byte, remoteBody []byte) ([]byte, bool, error) {
	if _, err := exec.LookPath("diff3"); err != nil {
		return nil, false, errors.New("diff3 is required for --merge but was not found in PATH")
	}

	tempDir, err := os.MkdirTemp("", "ob1-diff3-*")
	if err != nil {
		return nil, false, fmt.Errorf("create diff3 temp dir: %w", err)
	}
	defer os.RemoveAll(tempDir)

	files := []struct {
		name string
		body []byte
	}{
		{name: "local", body: localBody},
		{name: "base", body: baseBody},
		{name: "remote", body: remoteBody},
	}

	paths := make([]string, 0, len(files))
	for _, file := range files {
		path := filepath.Join(tempDir, file.name)
		if err := os.WriteFile(path, file.body, 0o600); err != nil {
			return nil, false, fmt.Errorf("write %s: %w", path, err)
		}
		paths = append(paths, path)
	}

	cmd := exec.Command("diff3", "-m", "-L", "local", "-L", "base", "-L", "remote", paths[0], paths[1], paths[2])
	output, err := cmd.CombinedOutput()
	if err == nil {
		return output, false, nil
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return output, true, nil
	}

	return nil, false, fmt.Errorf("run diff3: %w: %s", err, strings.TrimSpace(string(output)))
}

func logDryRunPullAction(logger *slog.Logger, path string, entry remotelist.Entry, backup *backupSession) error {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		logger.Info("would fetch file", "path", path)
		return nil
	case err != nil:
		return fmt.Errorf("lstat %s: %w", path, err)
	}

	if info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("%s is a directory", path)
	}

	if backup != nil {
		logger.Warn("would overwrite local file", "path", path, "backup", backup.target(path))
	} else {
		logger.Warn("would overwrite local file without backup", "path", path)
	}

	return nil
}

func logLocalMatchSummary(logger *slog.Logger, alreadyUpToDate int, metadataUpdated int, basesCopied int) {
	if alreadyUpToDate == 0 && metadataUpdated == 0 && basesCopied == 0 {
		return
	}

	logger.Info(
		"local file status",
		"already_up_to_date", alreadyUpToDate,
		"metadata_updated", metadataUpdated,
		"bases_copied", basesCopied,
	)
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

func mergeBasePath(path string) string {
	return filepath.Join(baseDirName, path)
}

func readMergeBase(path string) ([]byte, bool, error) {
	body, err := os.ReadFile(mergeBasePath(path))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read merge base for %s: %w", path, err)
	}

	return body, true, nil
}

func writeMergeBase(path string, body []byte) (bool, error) {
	basePath := mergeBasePath(path)
	existing, err := os.ReadFile(basePath)
	switch {
	case err == nil:
		if bytes.Equal(existing, body) {
			return false, nil
		}
	case errors.Is(err, os.ErrNotExist):
	default:
		return false, fmt.Errorf("read merge base for %s: %w", path, err)
	}

	if err := writeFileAtomically(basePath, body, baseDirPerm, baseFilePerm); err != nil {
		return false, err
	}

	return true, nil
}

func ensureMergeBaseFromLocal(path string, expectedHash string) (bool, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("read %s for merge base: %w", path, err)
	}
	if expectedHash != "" && vaultcrypto.PlaintextHash(body) != expectedHash {
		return false, nil
	}

	return writeMergeBase(path, body)
}

func removeMergeBase(path string) error {
	err := os.Remove(mergeBasePath(path))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove merge base for %s: %w", path, err)
	}

	return nil
}

func writeFileAtomically(path string, body []byte, dirPerm os.FileMode, filePerm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return fmt.Errorf("create directory for %s: %w", path, err)
	}

	tempFile, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file for %s: %w", path, err)
	}

	tempPath := tempFile.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tempPath)
		}
	}()

	if err := tempFile.Chmod(filePerm); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("chmod temp file for %s: %w", path, err)
	}
	if _, err := tempFile.Write(body); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("write temp file for %s: %w", path, err)
	}
	if err := tempFile.Sync(); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("sync temp file for %s: %w", path, err)
	}
	if err := tempFile.Close(); err != nil {
		return fmt.Errorf("close temp file for %s: %w", path, err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}

	cleanup = false
	return nil
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

func newBackupSession(enabled bool) *backupSession {
	if !enabled {
		return nil
	}

	return &backupSession{
		root: filepath.Join(".ob1", "backup", time.Now().UTC().Format("2006-01-02T15-04-05.000000Z")),
	}
}

func (b *backupSession) target(path string) string {
	if b == nil {
		return ""
	}

	return filepath.Join(b.root, path)
}

func (b *backupSession) enabled() bool {
	return b != nil
}

func (b *backupSession) move(path string) (string, error) {
	if b == nil {
		return "", nil
	}

	target := b.target(path)
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return "", fmt.Errorf("create backup directory for %s: %w", path, err)
	}
	if err := os.Rename(path, target); err != nil {
		return "", fmt.Errorf("move %s to backup: %w", path, err)
	}

	return target, nil
}

func writePulledFile(logger *slog.Logger, path string, file remotelist.File, backup *backupSession) (localFileWriteStatus, error) {
	dir := filepath.Dir(path)
	if dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return localFileUnchanged, fmt.Errorf("create directories for %s: %w", path, err)
		}
	}

	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.WriteFile(path, file.Body, 0o644); err != nil {
			return localFileUnchanged, fmt.Errorf("write %s: %w", path, err)
		}
		if _, err := updateLocalFileTimes(path, file.Entry.MTime); err != nil {
			return localFileUnchanged, err
		}
		return localFileContentUpdated, nil
	case err != nil:
		return localFileUnchanged, fmt.Errorf("lstat %s: %w", path, err)
	}

	if info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
		return localFileUnchanged, fmt.Errorf("%s is a directory", path)
	}

	if info.Mode()&os.ModeSymlink == 0 {
		existingBody, err := os.ReadFile(path)
		if err != nil {
			return localFileUnchanged, fmt.Errorf("read %s: %w", path, err)
		}
		if bytes.Equal(existingBody, file.Body) {
			updated, err := updateLocalFileTimes(path, file.Entry.MTime)
			if err != nil {
				return localFileUnchanged, err
			}
			if updated {
				return localFileMetadataOnly, nil
			}
			return localFileUnchanged, nil
		}
	}

	if backup != nil {
		backupPath, err := backup.move(path)
		if err != nil {
			return localFileUnchanged, err
		}
		logger.Warn("overwriting local file", "path", path, "backup", backupPath)
	} else {
		logger.Warn("overwriting local file without backup", "path", path)
	}

	if err := os.WriteFile(path, file.Body, 0o644); err != nil {
		return localFileUnchanged, fmt.Errorf("write %s: %w", path, err)
	}
	if _, err := updateLocalFileTimes(path, file.Entry.MTime); err != nil {
		return localFileUnchanged, err
	}

	return localFileContentUpdated, nil
}

func writeMergedFile(logger *slog.Logger, path string, file remotelist.File, backup *backupSession) (localFileWriteStatus, bool, error) {
	dir := filepath.Dir(path)
	if dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return localFileUnchanged, false, fmt.Errorf("create directories for %s: %w", path, err)
		}
	}

	baseBody, hasBase, err := readMergeBase(path)
	if err != nil {
		return localFileUnchanged, false, err
	}

	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.WriteFile(path, file.Body, 0o644); err != nil {
			return localFileUnchanged, false, fmt.Errorf("write %s: %w", path, err)
		}
		if _, err := updateLocalFileTimes(path, file.Entry.MTime); err != nil {
			return localFileUnchanged, false, err
		}
		baseCopied, err := writeMergeBase(path, file.Body)
		if err != nil {
			return localFileUnchanged, false, err
		}
		return localFileContentUpdated, baseCopied, nil
	case err != nil:
		return localFileUnchanged, false, fmt.Errorf("lstat %s: %w", path, err)
	}

	if info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
		return localFileUnchanged, false, fmt.Errorf("%s is a directory", path)
	}

	if info.Mode()&os.ModeSymlink != 0 {
		if err := replaceExistingWithBackup(logger, path, file.Body, file.Entry.MTime, backup, "overwriting symlinked local path"); err != nil {
			return localFileUnchanged, false, err
		}
		baseCopied, err := writeMergeBase(path, file.Body)
		if err != nil {
			return localFileUnchanged, false, err
		}
		return localFileContentUpdated, baseCopied, nil
	}

	localBody, err := os.ReadFile(path)
	if err != nil {
		return localFileUnchanged, false, fmt.Errorf("read %s: %w", path, err)
	}

	if bytes.Equal(localBody, file.Body) {
		updated, err := updateLocalFileTimes(path, file.Entry.MTime)
		if err != nil {
			return localFileUnchanged, false, err
		}
		baseCopied, err := writeMergeBase(path, file.Body)
		if err != nil {
			return localFileUnchanged, false, err
		}
		if updated {
			return localFileMetadataOnly, baseCopied, nil
		}
		return localFileUnchanged, baseCopied, nil
	}

	if !hasBase {
		if isTextFileForMerge(path, localBody, nil, file.Body) {
			conflictBody := twoWayConflict(localBody, file.Body)
			if err := replaceExistingWithBackup(logger, path, conflictBody, file.Entry.MTime, backup, "writing conflict markers without merge base"); err != nil {
				return localFileUnchanged, false, err
			}
			baseCopied, err := writeMergeBase(path, file.Body)
			if err != nil {
				return localFileUnchanged, false, err
			}
			return localFileConflict, baseCopied, nil
		}

		if err := replaceExistingWithBackup(logger, path, file.Body, file.Entry.MTime, backup, "overwriting local file without merge base"); err != nil {
			return localFileUnchanged, false, err
		}
		baseCopied, err := writeMergeBase(path, file.Body)
		if err != nil {
			return localFileUnchanged, false, err
		}
		return localFileContentUpdated, baseCopied, nil
	}

	if bytes.Equal(baseBody, file.Body) {
		return localFileKeptLocal, false, nil
	}

	if bytes.Equal(baseBody, localBody) {
		if err := replaceExistingWithBackup(logger, path, file.Body, file.Entry.MTime, backup, "overwriting local file"); err != nil {
			return localFileUnchanged, false, err
		}
		baseCopied, err := writeMergeBase(path, file.Body)
		if err != nil {
			return localFileUnchanged, false, err
		}
		return localFileContentUpdated, baseCopied, nil
	}

	if !isTextFileForMerge(path, localBody, baseBody, file.Body) {
		if err := replaceExistingWithBackup(logger, path, file.Body, file.Entry.MTime, backup, "overwriting binary file with divergent local and remote changes"); err != nil {
			return localFileUnchanged, false, err
		}
		baseCopied, err := writeMergeBase(path, file.Body)
		if err != nil {
			return localFileUnchanged, false, err
		}
		return localFileContentUpdated, baseCopied, nil
	}

	mergedBody, conflicted, err := diff3Merge(localBody, baseBody, file.Body)
	if err != nil {
		return localFileUnchanged, false, err
	}

	if err := replaceExistingWithBackup(logger, path, mergedBody, file.Entry.MTime, backup, "writing merged file"); err != nil {
		return localFileUnchanged, false, err
	}
	baseCopied, err := writeMergeBase(path, file.Body)
	if err != nil {
		return localFileUnchanged, false, err
	}

	if conflicted {
		return localFileConflict, baseCopied, nil
	}

	return localFileMerged, baseCopied, nil
}

func isTextFileForMerge(path string, localBody []byte, baseBody []byte, remoteBody []byte) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".txt", ".json", ".yaml", ".yml", ".toml", ".ini", ".cfg", ".conf", ".csv", ".tsv",
		".html", ".htm", ".xml", ".css", ".js", ".mjs", ".cjs", ".ts", ".tsx", ".jsx",
		".go", ".py", ".rb", ".rs", ".java", ".kt", ".swift", ".c", ".cc", ".cpp", ".cxx",
		".h", ".hh", ".hpp", ".hxx", ".sh", ".bash", ".zsh", ".fish", ".sql", ".graphql":
		return true
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".ico", ".pdf", ".zip", ".gz", ".tgz",
		".xz", ".bz2", ".7z", ".rar", ".mp3", ".m4a", ".wav", ".ogg", ".mp4", ".mov", ".avi",
		".mkv", ".webm", ".woff", ".woff2", ".ttf", ".otf", ".eot", ".sqlite", ".db", ".bin":
		return false
	}

	for _, body := range [][]byte{localBody, baseBody, remoteBody} {
		if len(body) == 0 {
			continue
		}
		if !utf8.Valid(body) {
			return false
		}
	}

	return true
}

func twoWayConflict(localBody []byte, remoteBody []byte) []byte {
	var b bytes.Buffer
	b.WriteString("<<<<<<< local\n")
	b.Write(localBody)
	if len(localBody) != 0 && localBody[len(localBody)-1] != '\n' {
		b.WriteByte('\n')
	}
	b.WriteString("=======\n")
	b.Write(remoteBody)
	if len(remoteBody) != 0 && remoteBody[len(remoteBody)-1] != '\n' {
		b.WriteByte('\n')
	}
	b.WriteString(">>>>>>> remote\n")

	return b.Bytes()
}

func replaceExistingWithBackup(logger *slog.Logger, path string, body []byte, mtimeMs int64, backup *backupSession, message string) error {
	if backup != nil {
		backupPath, err := backup.move(path)
		if err != nil {
			return err
		}
		logger.Warn(message, "path", path, "backup", backupPath)
	} else {
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove %s: %w", path, err)
		}
		logger.Warn(message+" without backup", "path", path)
	}

	if err := os.WriteFile(path, body, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if _, err := updateLocalFileTimes(path, mtimeMs); err != nil {
		return err
	}

	return nil
}

func listUnknownLocalFiles(logger *slog.Logger, entries []remotelist.Entry) ([]string, error) {
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

	unknown := make([]string, 0)
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

		unknown = append(unknown, relPath)

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan local files for deletion: %w", err)
	}

	sort.Strings(unknown)

	return unknown, nil
}

func deleteUnknownLocalFiles(logger *slog.Logger, entries []remotelist.Entry, backup *backupSession) (int, error) {
	unknown, err := listUnknownLocalFiles(logger, entries)
	if err != nil {
		return 0, err
	}

	for _, path := range unknown {
		if backup != nil {
			backupPath, err := backup.move(path)
			if err != nil {
				return 0, err
			}
			logger.Warn("deleting unknown local file", "path", path, "backup", backupPath)
		} else {
			logger.Warn("deleting unknown local file without backup", "path", path)
			if err := os.Remove(path); err != nil {
				return 0, fmt.Errorf("remove %s: %w", path, err)
			}
		}
		if err := removeMergeBase(path); err != nil {
			return 0, err
		}
	}

	return len(unknown), nil
}

func logDryRunDeleteUnknown(logger *slog.Logger, entries []remotelist.Entry, backup *backupSession) (int, error) {
	unknown, err := listUnknownLocalFiles(logger, entries)
	if err != nil {
		return 0, err
	}

	for _, path := range unknown {
		if backup != nil {
			logger.Warn("would delete unknown local file", "path", path, "backup", backup.target(path))
		} else {
			logger.Warn("would delete unknown local file without backup", "path", path)
		}
	}

	return len(unknown), nil
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
