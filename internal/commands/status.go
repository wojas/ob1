package commands

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/fatih/color"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/wojas/ob1/internal/remotelist"
	"github.com/wojas/ob1/internal/vaultcrypto"
	"github.com/wojas/ob1/internal/vaultstore"
)

type localStatusEntry struct {
	path    string
	hash    string
	mtimeMs int64
	symlink bool
}

type fileStatusRow struct {
	path         string
	localStatus  string
	remoteStatus string
}

func NewStatusCommand(rt Runtime, debug *bool, noCache *bool) *cobra.Command {
	var all bool
	var verbose bool

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show local vs remote file status",
		RunE: func(cmd *cobra.Command, _ []string) error {
			logger := rt.NewLogger(*debug)

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

			cacheStore := remotelist.NewCacheStore(".")
			cached, err := loadRemoteCache(cacheStore, *noCache)
			if err != nil {
				return err
			}

			snapshot, err := remotelist.SyncEntries(cmd.Context(), logger, userState.Token, vaultState, cached, !*noCache)
			if err != nil {
				return err
			}

			if err := maybeSaveRemoteCache(cacheStore, cached, snapshot, effectiveNoCache(*noCache, rt.IsDryRun())); err != nil {
				return err
			}

			localEntries, err := scanLocalStatusEntries()
			if err != nil {
				return err
			}

			rows := buildStatusRows(localEntries, snapshot.Entries)
			if !all {
				rows = filterChangedStatusRows(rows)
				if len(rows) == 0 {
					logger.Info("no local or remote differences")
					return nil
				}
			}

			return writeStatusTable(os.Stdout, rows, verbose)
		},
	}

	cmd.Flags().BoolVarP(&all, "all", "a", false, "show files that are already in sync")
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "show human-readable status descriptions")

	return cmd
}

func scanLocalStatusEntries() (map[string]localStatusEntry, error) {
	entries := make(map[string]localStatusEntry)

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

		relPath := filepath.Clean(path)
		if !isStatusVisiblePath(relPath) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if d.Type()&os.ModeSymlink != 0 {
			entries[relPath] = localStatusEntry{
				path:    relPath,
				symlink: true,
			}
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if d.IsDir() {
			return nil
		}

		body, err := os.ReadFile(relPath)
		if err != nil {
			return fmt.Errorf("read %s: %w", relPath, err)
		}

		info, err := os.Stat(relPath)
		if err != nil {
			return fmt.Errorf("stat %s: %w", relPath, err)
		}

		entries[relPath] = localStatusEntry{
			path:    relPath,
			hash:    vaultcrypto.PlaintextHash(body),
			mtimeMs: info.ModTime().UnixMilli(),
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan local status: %w", err)
	}

	return entries, nil
}

func buildStatusRows(localEntries map[string]localStatusEntry, remoteEntries []remotelist.Entry) []fileStatusRow {
	rowsByPath := make(map[string]fileStatusRow)

	for path, local := range localEntries {
		if local.symlink {
			rowsByPath[path] = fileStatusRow{
				path:         path,
				localStatus:  "symlink",
				remoteStatus: "missing",
			}
			continue
		}

		rowsByPath[path] = fileStatusRow{
			path:         path,
			localStatus:  "only-local",
			remoteStatus: "missing",
		}
	}

	for _, remote := range remoteEntries {
		if remote.Folder {
			continue
		}

		path, ok := safeLocalTarget(remote.Path)
		if !ok || !isStatusVisiblePath(path) {
			continue
		}

		local, hasLocal := localEntries[path]
		if !hasLocal {
			rowsByPath[path] = fileStatusRow{
				path:         path,
				localStatus:  "missing",
				remoteStatus: "only-remote",
			}
			continue
		}

		if local.symlink {
			rowsByPath[path] = fileStatusRow{
				path:         path,
				localStatus:  "symlink",
				remoteStatus: "present",
			}
			continue
		}

		if remote.Hash != "" && local.hash == remote.Hash {
			row := fileStatusRow{
				path:         path,
				localStatus:  "same",
				remoteStatus: "same",
			}
			if remote.MTime > 0 && local.mtimeMs != remote.MTime {
				row.localStatus = "metadata"
				row.remoteStatus = "metadata"
			}
			rowsByPath[path] = row
			continue
		}

		rowsByPath[path] = fileStatusRow{
			path:         path,
			localStatus:  "different",
			remoteStatus: "different",
		}
	}

	rows := make([]fileStatusRow, 0, len(rowsByPath))
	for _, row := range rowsByPath {
		rows = append(rows, row)
	}

	sort.Slice(rows, func(i, j int) bool {
		return rows[i].path < rows[j].path
	})

	return rows
}

func filterChangedStatusRows(rows []fileStatusRow) []fileStatusRow {
	filtered := make([]fileStatusRow, 0, len(rows))
	for _, row := range rows {
		if row.localStatus == "same" && row.remoteStatus == "same" {
			continue
		}
		filtered = append(filtered, row)
	}

	return filtered
}

func writeStatusTable(out io.Writer, rows []fileStatusRow, verbose bool) error {
	useColor := false
	if file, ok := out.(*os.File); ok {
		useColor = term.IsTerminal(int(file.Fd()))
	}

	if len(rows) == 0 {
		if _, err := fmt.Fprintln(out, "(none)"); err != nil {
			return fmt.Errorf("write empty status: %w", err)
		}
		return nil
	}

	for _, row := range rows {
		prefix := formatStatusPrefix(row, useColor)
		if verbose {
			description := "(" + statusDescription(row) + ")"
			if useColor {
				description = renderColor(description, color.Faint)
			}
			if _, err := fmt.Fprintf(out, "%s %s   %s\n", prefix, row.path, description); err != nil {
				return fmt.Errorf("write status row: %w", err)
			}
			continue
		}

		if _, err := fmt.Fprintf(out, "%s %s\n", prefix, row.path); err != nil {
			return fmt.Errorf("write status row: %w", err)
		}
	}

	return nil
}

func statusCode(status string) byte {
	switch status {
	case "same":
		return '='
	case "metadata":
		return 'T'
	case "different":
		return 'M'
	case "only-local", "only-remote":
		return 'A'
	case "missing":
		return '.'
	case "symlink":
		return '?'
	case "present":
		return 'A'
	default:
		return '?'
	}
}

func formatStatusPrefix(row fileStatusRow, color bool) string {
	local := string(statusCode(row.localStatus))
	remote := string(statusCode(row.remoteStatus))
	if !color {
		return local + remote
	}

	return colorizeStatus(local, row.localStatus, row.remoteStatus, true) + colorizeStatus(remote, row.remoteStatus, row.localStatus, false)
}

func colorizeStatus(code string, status string, otherStatus string, local bool) string {
	attrs := []color.Attribute{}
	switch status {
	case "same":
		attrs = append(attrs, color.FgGreen)
	case "metadata":
		attrs = append(attrs, color.FgCyan)
	case "different":
		attrs = append(attrs, color.FgYellow)
	case "only-local":
		attrs = append(attrs, color.FgMagenta)
	case "only-remote", "present":
		attrs = append(attrs, color.FgCyan)
	case "missing":
		switch otherStatus {
		case "only-local":
			attrs = append(attrs, color.Faint, color.FgMagenta)
		case "only-remote", "present":
			attrs = append(attrs, color.Faint, color.FgCyan)
		default:
			if local {
				attrs = append(attrs, color.Faint, color.FgWhite)
			} else {
				attrs = append(attrs, color.Faint, color.FgWhite)
			}
		}
	case "symlink":
		attrs = append(attrs, color.FgMagenta)
	default:
		attrs = append(attrs, color.FgWhite)
	}

	return renderColor(code, attrs...)
}

func renderColor(text string, attrs ...color.Attribute) string {
	c := color.New(attrs...)
	return c.Sprint(text)
}

func statusDescription(row fileStatusRow) string {
	switch {
	case row.localStatus == "same" && row.remoteStatus == "same":
		return "in sync"
	case row.localStatus == "metadata" && row.remoteStatus == "metadata":
		return "content matches, timestamps differ"
	case row.localStatus == "different" && row.remoteStatus == "different":
		return "different locally and on server"
	case row.localStatus == "only-local" && row.remoteStatus == "missing":
		return "present only locally"
	case row.localStatus == "missing" && row.remoteStatus == "only-remote":
		return "present only on server"
	case row.localStatus == "symlink" && row.remoteStatus == "present":
		return "local path is a symlink, server has a file"
	case row.localStatus == "symlink" && row.remoteStatus == "missing":
		return "local path is a symlink"
	default:
		return row.localStatus + " / " + row.remoteStatus
	}
}

func isStatusVisiblePath(path string) bool {
	cleaned := filepath.Clean(path)
	if cleaned == "." || cleaned == "" {
		return false
	}

	for _, part := range strings.Split(cleaned, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		if strings.HasPrefix(part, ".") {
			return false
		}
	}

	return true
}
