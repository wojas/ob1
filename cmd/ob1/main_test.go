package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wojas/ob1/internal/testserver"
)

type cliHarness struct {
	t       *testing.T
	homeDir string
	workDir string
}

type cliResult struct {
	code   int
	stdout string
	stderr string
}

func TestCLIWorkflowAgainstTestServer(t *testing.T) {
	server, err := testserver.New(testserver.Options{
		InitialFiles: map[string][]byte{
			"docs/readme.txt": []byte("remote docs\n"),
			"notes/todo.md":   []byte("# Remote Todo\n"),
		},
	})
	if err != nil {
		t.Fatalf("start test server: %v", err)
	}
	t.Cleanup(server.Close)

	cli := newCLIHarness(t, server.APIBaseURL())

	cli.run("login", "--email", server.Email(), "--password", server.AccountPassword()).requireSuccess(t)

	info := cli.run("info")
	info.requireSuccess(t)
	if !strings.Contains(info.stdout, server.Email()) {
		t.Fatalf("info output did not contain email %q:\n%s", server.Email(), info.stdout)
	}

	vaults := cli.run("vault", "list")
	vaults.requireSuccess(t)
	if !strings.Contains(vaults.stdout, server.VaultID()) {
		t.Fatalf("vault list did not contain vault id %q:\n%s", server.VaultID(), vaults.stdout)
	}

	cli.run(
		"vault",
		"setup",
		server.VaultID(),
		"--vault-password",
		server.VaultPassword(),
		"--device-name",
		"test-device",
	).requireSuccess(t)

	remoteList := cli.run("list")
	remoteList.requireSuccess(t)
	if !strings.Contains(remoteList.stdout, "notes/todo.md") || !strings.Contains(remoteList.stdout, "docs/readme.txt") {
		t.Fatalf("list output did not contain expected files:\n%s", remoteList.stdout)
	}

	cat := cli.run("cat", "notes/todo.md")
	cat.requireSuccess(t)
	if cat.stdout != "# Remote Todo\n" {
		t.Fatalf("unexpected cat output: %q", cat.stdout)
	}

	cli.run("get", "--merge", "notes/todo.md").requireSuccess(t)

	noteBody, err := os.ReadFile(filepath.Join(cli.workDir, "notes", "todo.md"))
	if err != nil {
		t.Fatalf("read fetched note: %v", err)
	}
	if string(noteBody) != "# Remote Todo\n" {
		t.Fatalf("unexpected fetched note body: %q", string(noteBody))
	}

	if _, err := os.Stat(filepath.Join(cli.workDir, ".ob1", "base", "notes", "todo.md")); err != nil {
		t.Fatalf("merge base not written: %v", err)
	}

	status := cli.run("status")
	status.requireSuccess(t)
	if strings.Contains(status.stdout, "notes/todo.md") {
		t.Fatalf("status should not include in-sync file:\n%s", status.stdout)
	}
	if !strings.Contains(status.stdout, "docs/readme.txt") {
		t.Fatalf("status should include remote-only file:\n%s", status.stdout)
	}

	if err := os.MkdirAll(filepath.Join(cli.workDir, "upload"), 0o755); err != nil {
		t.Fatalf("create upload dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cli.workDir, "upload", "new.md"), []byte("new upload\n"), 0o644); err != nil {
		t.Fatalf("write upload file: %v", err)
	}

	cli.run("put", "upload/new.md").requireSuccess(t)

	uploadedBody, ok := server.FileBody("upload/new.md")
	if !ok {
		t.Fatal("server did not receive uploaded file")
	}
	if string(uploadedBody) != "new upload\n" {
		t.Fatalf("unexpected uploaded body: %q", string(uploadedBody))
	}

	if err := os.WriteFile(filepath.Join(cli.workDir, "scratch.txt"), []byte("delete me\n"), 0o644); err != nil {
		t.Fatalf("write scratch file: %v", err)
	}

	cli.run("pull", "--delete-unknown").requireSuccess(t)

	docsBody, err := os.ReadFile(filepath.Join(cli.workDir, "docs", "readme.txt"))
	if err != nil {
		t.Fatalf("read pulled docs file: %v", err)
	}
	if string(docsBody) != "remote docs\n" {
		t.Fatalf("unexpected pulled docs body: %q", string(docsBody))
	}

	if _, err := os.Stat(filepath.Join(cli.workDir, "scratch.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("scratch.txt was not deleted, err=%v", err)
	}

	backupMatches, err := filepath.Glob(filepath.Join(cli.workDir, ".ob1", "backup", "*", "scratch.txt"))
	if err != nil {
		t.Fatalf("glob backup files: %v", err)
	}
	if len(backupMatches) != 1 {
		t.Fatalf("expected one backup for scratch.txt, found %d", len(backupMatches))
	}

	fullStatus := cli.run("status", "-a")
	fullStatus.requireSuccess(t)
	for _, want := range []string{"notes/todo.md", "docs/readme.txt", "upload/new.md"} {
		if !strings.Contains(fullStatus.stdout, want) {
			t.Fatalf("status -a missing %q:\n%s", want, fullStatus.stdout)
		}
	}

	cli.run("logout").requireSuccess(t)

	if _, err := os.Stat(filepath.Join(cli.homeDir, ".ob1", "user.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("user store still exists after logout, err=%v", err)
	}
	if server.SignOutCalls() != 1 {
		t.Fatalf("unexpected remote signout count: %d", server.SignOutCalls())
	}
}

func TestPullDryRunDoesNotWriteState(t *testing.T) {
	server, err := testserver.New(testserver.Options{
		InitialFiles: map[string][]byte{
			"remote.md": []byte("dry run payload\n"),
		},
	})
	if err != nil {
		t.Fatalf("start test server: %v", err)
	}
	t.Cleanup(server.Close)

	cli := newCLIHarness(t, server.APIBaseURL())

	cli.run("login", "--email", server.Email(), "--password", server.AccountPassword()).requireSuccess(t)
	cli.run(
		"vault",
		"setup",
		server.VaultID(),
		"--vault-password",
		server.VaultPassword(),
		"--device-name",
		"test-device",
	).requireSuccess(t)

	cli.run("--dry-run", "pull").requireSuccess(t)

	if _, err := os.Stat(filepath.Join(cli.workDir, "remote.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remote file was written during dry-run, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(cli.workDir, ".ob1", "cache.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cache was written during dry-run, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(cli.workDir, ".ob1", "base")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("merge base directory was created during dry-run, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(cli.workDir, ".ob1", "backup")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("backup directory was created during dry-run, err=%v", err)
	}
}

func TestGetMergePerformsCleanThreeWayMerge(t *testing.T) {
	requireDiff3(t)

	server, err := testserver.New(testserver.Options{
		InitialFiles: map[string][]byte{
			"merge.txt": []byte("alpha\nshared\nomega\n"),
		},
	})
	if err != nil {
		t.Fatalf("start test server: %v", err)
	}
	t.Cleanup(server.Close)

	cli := newCLIHarness(t, server.APIBaseURL())
	loginAndSetup(t, cli, server)
	cli.run("get", "--merge", "merge.txt").requireSuccess(t)

	if err := os.WriteFile(filepath.Join(cli.workDir, "merge.txt"), []byte("alpha local\nshared\nomega\n"), 0o644); err != nil {
		t.Fatalf("write local merge candidate: %v", err)
	}
	if err := server.SetFileBody("merge.txt", []byte("alpha\nshared\nomega remote\n")); err != nil {
		t.Fatalf("update remote file: %v", err)
	}

	cli.run("get", "--merge", "merge.txt").requireSuccess(t)

	mergedBody, err := os.ReadFile(filepath.Join(cli.workDir, "merge.txt"))
	if err != nil {
		t.Fatalf("read merged file: %v", err)
	}
	if string(mergedBody) != "alpha local\nshared\nomega remote\n" {
		t.Fatalf("unexpected merged content:\n%s", string(mergedBody))
	}
	if strings.Contains(string(mergedBody), "<<<<<<<") {
		t.Fatalf("clean merge should not contain conflict markers:\n%s", string(mergedBody))
	}

	baseBody, err := os.ReadFile(filepath.Join(cli.workDir, ".ob1", "base", "merge.txt"))
	if err != nil {
		t.Fatalf("read merge base: %v", err)
	}
	if string(baseBody) != "alpha\nshared\nomega remote\n" {
		t.Fatalf("merge base was not refreshed to remote content:\n%s", string(baseBody))
	}
}

func TestPullMergeWritesConflictMarkersAndBackup(t *testing.T) {
	requireDiff3(t)

	server, err := testserver.New(testserver.Options{
		InitialFiles: map[string][]byte{
			"conflict.txt": []byte("one\nshared\nthree\n"),
		},
	})
	if err != nil {
		t.Fatalf("start test server: %v", err)
	}
	t.Cleanup(server.Close)

	cli := newCLIHarness(t, server.APIBaseURL())
	loginAndSetup(t, cli, server)
	cli.run("get", "--merge", "conflict.txt").requireSuccess(t)

	if err := os.WriteFile(filepath.Join(cli.workDir, "conflict.txt"), []byte("one\nlocal change\nthree\n"), 0o644); err != nil {
		t.Fatalf("write local conflict file: %v", err)
	}
	if err := server.SetFileBody("conflict.txt", []byte("one\nremote change\nthree\n")); err != nil {
		t.Fatalf("update remote conflict file: %v", err)
	}

	cli.run("pull", "--merge").requireSuccess(t)

	conflictedBody, err := os.ReadFile(filepath.Join(cli.workDir, "conflict.txt"))
	if err != nil {
		t.Fatalf("read conflicted file: %v", err)
	}
	got := string(conflictedBody)
	for _, want := range []string{
		"<<<<<<< local",
		"||||||| base",
		"local change",
		"remote change",
		">>>>>>> remote",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("conflicted merge output missing %q:\n%s", want, got)
		}
	}

	backupMatches, err := filepath.Glob(filepath.Join(cli.workDir, ".ob1", "backup", "*", "conflict.txt"))
	if err != nil {
		t.Fatalf("glob conflict backups: %v", err)
	}
	if len(backupMatches) != 1 {
		t.Fatalf("expected one backup for conflict.txt, found %d", len(backupMatches))
	}

	backupBody, err := os.ReadFile(backupMatches[0])
	if err != nil {
		t.Fatalf("read conflict backup: %v", err)
	}
	if string(backupBody) != "one\nlocal change\nthree\n" {
		t.Fatalf("unexpected backup content:\n%s", string(backupBody))
	}

	baseBody, err := os.ReadFile(filepath.Join(cli.workDir, ".ob1", "base", "conflict.txt"))
	if err != nil {
		t.Fatalf("read conflict merge base: %v", err)
	}
	if string(baseBody) != "one\nremote change\nthree\n" {
		t.Fatalf("conflict merge base was not refreshed to remote content:\n%s", string(baseBody))
	}
}

func TestPullMergeKeepsLocalChangesWhenRemoteMatchesMergeBase(t *testing.T) {
	requireDiff3(t)

	server, err := testserver.New(testserver.Options{
		InitialFiles: map[string][]byte{
			"keep.txt": []byte("base line\n"),
		},
	})
	if err != nil {
		t.Fatalf("start test server: %v", err)
	}
	t.Cleanup(server.Close)

	cli := newCLIHarness(t, server.APIBaseURL())
	loginAndSetup(t, cli, server)
	cli.run("get", "--merge", "keep.txt").requireSuccess(t)

	if err := os.WriteFile(filepath.Join(cli.workDir, "keep.txt"), []byte("local only change\n"), 0o644); err != nil {
		t.Fatalf("write local keep file: %v", err)
	}

	result := cli.run("pull", "--merge")
	result.requireSuccess(t)

	keptBody, err := os.ReadFile(filepath.Join(cli.workDir, "keep.txt"))
	if err != nil {
		t.Fatalf("read kept file: %v", err)
	}
	if string(keptBody) != "local only change\n" {
		t.Fatalf("local change was not preserved:\n%s", string(keptBody))
	}

	baseBody, err := os.ReadFile(filepath.Join(cli.workDir, ".ob1", "base", "keep.txt"))
	if err != nil {
		t.Fatalf("read keep merge base: %v", err)
	}
	if string(baseBody) != "base line\n" {
		t.Fatalf("merge base should remain unchanged when remote matches base:\n%s", string(baseBody))
	}

	if !strings.Contains(result.stderr, "keeping local changes") {
		t.Fatalf("expected keep-local log message, stderr:\n%s", result.stderr)
	}
	if _, err := os.Stat(filepath.Join(cli.workDir, ".ob1", "backup")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("backup directory should not be created when keeping local changes, err=%v", err)
	}
}

func TestMVMovesRemoteAndLocalWithBackup(t *testing.T) {
	server, err := testserver.New(testserver.Options{
		InitialFiles: map[string][]byte{
			"move.txt": []byte("remote original\n"),
		},
	})
	if err != nil {
		t.Fatalf("start test server: %v", err)
	}
	t.Cleanup(server.Close)

	cli := newCLIHarness(t, server.APIBaseURL())
	loginAndSetup(t, cli, server)
	cli.run("get", "--merge", "move.txt").requireSuccess(t)

	sourcePath := filepath.Join(cli.workDir, "move.txt")
	targetPath := filepath.Join(cli.workDir, "archive", "move-renamed.txt")
	if err := os.WriteFile(sourcePath, []byte("local changed\n"), 0o644); err != nil {
		t.Fatalf("write local source candidate: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		t.Fatalf("create local target parent: %v", err)
	}
	if err := os.WriteFile(targetPath, []byte("local target old\n"), 0o644); err != nil {
		t.Fatalf("write local target candidate: %v", err)
	}

	cli.run("experimental-mv", "move.txt", "archive/move-renamed.txt").requireSuccess(t)

	if _, ok := server.FileBody("move.txt"); ok {
		t.Fatal("remote source still exists after mv")
	}
	remoteBody, ok := server.FileBody("archive/move-renamed.txt")
	if !ok {
		t.Fatal("remote target missing after mv")
	}
	if string(remoteBody) != "remote original\n" {
		t.Fatalf("unexpected remote moved body: %q", string(remoteBody))
	}

	if _, err := os.Stat(sourcePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("local source was not moved, err=%v", err)
	}
	localMovedBody, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("read local moved target: %v", err)
	}
	if string(localMovedBody) != "local changed\n" {
		t.Fatalf("unexpected local moved body: %q", string(localMovedBody))
	}

	backupMatches, err := filepath.Glob(filepath.Join(cli.workDir, ".ob1", "backup", "*", "archive", "move-renamed.txt"))
	if err != nil {
		t.Fatalf("glob mv backups: %v", err)
	}
	if len(backupMatches) != 1 {
		t.Fatalf("expected one backup for archive/move-renamed.txt, found %d", len(backupMatches))
	}
	backupBody, err := os.ReadFile(backupMatches[0])
	if err != nil {
		t.Fatalf("read mv backup: %v", err)
	}
	if string(backupBody) != "local target old\n" {
		t.Fatalf("unexpected mv backup content: %q", string(backupBody))
	}

	if _, err := os.Stat(filepath.Join(cli.workDir, ".ob1", "base", "move.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source merge base still exists after mv, err=%v", err)
	}
	baseBody, err := os.ReadFile(filepath.Join(cli.workDir, ".ob1", "base", "archive", "move-renamed.txt"))
	if err != nil {
		t.Fatalf("read target merge base: %v", err)
	}
	if string(baseBody) != "remote original\n" {
		t.Fatalf("unexpected target merge base content: %q", string(baseBody))
	}
}

func TestMVDryRunDoesNotModifyState(t *testing.T) {
	server, err := testserver.New(testserver.Options{
		InitialFiles: map[string][]byte{
			"dry-mv.txt": []byte("remote original\n"),
		},
	})
	if err != nil {
		t.Fatalf("start test server: %v", err)
	}
	t.Cleanup(server.Close)

	cli := newCLIHarness(t, server.APIBaseURL())
	loginAndSetup(t, cli, server)
	cli.run("get", "--merge", "dry-mv.txt").requireSuccess(t)

	sourcePath := filepath.Join(cli.workDir, "dry-mv.txt")
	targetPath := filepath.Join(cli.workDir, "archive", "dry-mv-renamed.txt")
	if err := os.WriteFile(sourcePath, []byte("local changed\n"), 0o644); err != nil {
		t.Fatalf("write local dry-run source candidate: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		t.Fatalf("create dry-run target parent: %v", err)
	}
	if err := os.WriteFile(targetPath, []byte("local target old\n"), 0o644); err != nil {
		t.Fatalf("write dry-run target candidate: %v", err)
	}

	cachePath := filepath.Join(cli.workDir, ".ob1", "cache.json")
	cacheBefore, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("read cache before mv dry-run: %v", err)
	}

	cli.run("--dry-run", "experimental-mv", "dry-mv.txt", "archive/dry-mv-renamed.txt").requireSuccess(t)

	remoteSourceBody, ok := server.FileBody("dry-mv.txt")
	if !ok {
		t.Fatal("remote source was moved during mv --dry-run")
	}
	if string(remoteSourceBody) != "remote original\n" {
		t.Fatalf("unexpected remote source body after dry-run: %q", string(remoteSourceBody))
	}
	if _, ok := server.FileBody("archive/dry-mv-renamed.txt"); ok {
		t.Fatal("remote target was created during mv --dry-run")
	}

	localSourceBody, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatalf("read local source after mv dry-run: %v", err)
	}
	if string(localSourceBody) != "local changed\n" {
		t.Fatalf("local source changed during mv --dry-run: %q", string(localSourceBody))
	}
	localTargetBody, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("read local target after mv dry-run: %v", err)
	}
	if string(localTargetBody) != "local target old\n" {
		t.Fatalf("local target changed during mv --dry-run: %q", string(localTargetBody))
	}

	if _, err := os.Stat(filepath.Join(cli.workDir, ".ob1", "base", "dry-mv.txt")); err != nil {
		t.Fatalf("source merge base should remain after mv --dry-run, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(cli.workDir, ".ob1", "base", "archive", "dry-mv-renamed.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target merge base should not be created during mv --dry-run, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(cli.workDir, ".ob1", "backup")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("backup directory should not be created during mv --dry-run, err=%v", err)
	}

	cacheAfter, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("read cache after mv dry-run: %v", err)
	}
	if string(cacheAfter) != string(cacheBefore) {
		t.Fatal("cache changed during mv --dry-run")
	}
}

func TestMVRejectsDirectoryLikeDestination(t *testing.T) {
	server, err := testserver.New(testserver.Options{
		InitialFiles: map[string][]byte{
			"source.txt": []byte("remote source\n"),
		},
	})
	if err != nil {
		t.Fatalf("start test server: %v", err)
	}
	t.Cleanup(server.Close)

	cli := newCLIHarness(t, server.APIBaseURL())
	loginAndSetup(t, cli, server)
	cli.run("get", "--merge", "source.txt").requireSuccess(t)

	localSourcePath := filepath.Join(cli.workDir, "source.txt")
	if err := os.WriteFile(localSourcePath, []byte("local source\n"), 0o644); err != nil {
		t.Fatalf("write local source: %v", err)
	}

	result := cli.run("experimental-mv", "source.txt", "archive/")
	result.requireFailure(t)
	if !strings.Contains(result.stderr, "destination must include a file name") {
		t.Fatalf("expected directory-like destination validation error, stderr:\n%s", result.stderr)
	}

	remoteBody, ok := server.FileBody("source.txt")
	if !ok {
		t.Fatal("remote source file was changed despite validation failure")
	}
	if string(remoteBody) != "remote source\n" {
		t.Fatalf("unexpected remote source body after failed mv: %q", string(remoteBody))
	}

	localBody, err := os.ReadFile(localSourcePath)
	if err != nil {
		t.Fatalf("read local source after failed mv: %v", err)
	}
	if string(localBody) != "local source\n" {
		t.Fatalf("local source changed after failed mv: %q", string(localBody))
	}
}

func TestMVRejectsExistingDirectoryDestination(t *testing.T) {
	server, err := testserver.New(testserver.Options{
		InitialFiles: map[string][]byte{
			"source.txt":          []byte("remote source\n"),
			"archive/existing.md": []byte("remote sibling\n"),
		},
	})
	if err != nil {
		t.Fatalf("start test server: %v", err)
	}
	t.Cleanup(server.Close)

	cli := newCLIHarness(t, server.APIBaseURL())
	loginAndSetup(t, cli, server)
	cli.run("get", "--merge", "source.txt").requireSuccess(t)

	result := cli.run("experimental-mv", "source.txt", "archive")
	result.requireFailure(t)
	if !strings.Contains(result.stderr, "exists as a folder") {
		t.Fatalf("expected folder destination error, stderr:\n%s", result.stderr)
	}

	if _, ok := server.FileBody("archive/source.txt"); ok {
		t.Fatal("remote file moved into directory despite rejected destination")
	}
	if _, ok := server.FileBody("source.txt"); !ok {
		t.Fatal("remote source removed despite rejected destination")
	}
}

func TestMVHelpDocumentsSpecialCases(t *testing.T) {
	cli := newCLIHarness(t, "http://127.0.0.1:1")

	result := cli.run("experimental-mv", "--help")
	result.requireSuccess(t)

	for _, needle := range []string{
		"destination must be a file path",
		`"archive/" is rejected`,
		`ob1 experimental-mv note.md archive/note.md`,
	} {
		if !strings.Contains(result.stdout, needle) {
			t.Fatalf("mv help missing %q:\n%s", needle, result.stdout)
		}
	}
}

func TestRMRemovesRemoteAndLocalWithBackup(t *testing.T) {
	server, err := testserver.New(testserver.Options{
		InitialFiles: map[string][]byte{
			"remove.txt": []byte("remote original\n"),
		},
	})
	if err != nil {
		t.Fatalf("start test server: %v", err)
	}
	t.Cleanup(server.Close)

	cli := newCLIHarness(t, server.APIBaseURL())
	loginAndSetup(t, cli, server)
	cli.run("get", "--merge", "remove.txt").requireSuccess(t)

	localPath := filepath.Join(cli.workDir, "remove.txt")
	if err := os.WriteFile(localPath, []byte("local changed\n"), 0o644); err != nil {
		t.Fatalf("write local remove candidate: %v", err)
	}

	cli.run("rm", "remove.txt").requireSuccess(t)

	if _, ok := server.FileBody("remove.txt"); ok {
		t.Fatal("remote file still exists after rm")
	}
	if _, err := os.Stat(localPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("local file was not removed, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(cli.workDir, ".ob1", "base", "remove.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("merge base was not removed, err=%v", err)
	}

	backupMatches, err := filepath.Glob(filepath.Join(cli.workDir, ".ob1", "backup", "*", "remove.txt"))
	if err != nil {
		t.Fatalf("glob rm backups: %v", err)
	}
	if len(backupMatches) != 1 {
		t.Fatalf("expected one backup for remove.txt, found %d", len(backupMatches))
	}

	backupBody, err := os.ReadFile(backupMatches[0])
	if err != nil {
		t.Fatalf("read rm backup: %v", err)
	}
	if string(backupBody) != "local changed\n" {
		t.Fatalf("unexpected rm backup content: %q", string(backupBody))
	}
}

func TestRMDryRunDoesNotModifyState(t *testing.T) {
	server, err := testserver.New(testserver.Options{
		InitialFiles: map[string][]byte{
			"dry-rm.txt": []byte("remote original\n"),
		},
	})
	if err != nil {
		t.Fatalf("start test server: %v", err)
	}
	t.Cleanup(server.Close)

	cli := newCLIHarness(t, server.APIBaseURL())
	loginAndSetup(t, cli, server)
	cli.run("get", "--merge", "dry-rm.txt").requireSuccess(t)

	localPath := filepath.Join(cli.workDir, "dry-rm.txt")
	if err := os.WriteFile(localPath, []byte("local changed\n"), 0o644); err != nil {
		t.Fatalf("write local dry-run candidate: %v", err)
	}

	cachePath := filepath.Join(cli.workDir, ".ob1", "cache.json")
	cacheBefore, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("read cache before rm dry-run: %v", err)
	}

	result := cli.run("--dry-run", "rm", "dry-rm.txt")
	result.requireSuccess(t)

	remoteBody, ok := server.FileBody("dry-rm.txt")
	if !ok {
		t.Fatal("remote file was deleted during rm --dry-run")
	}
	if string(remoteBody) != "remote original\n" {
		t.Fatalf("unexpected remote body after rm --dry-run: %q", string(remoteBody))
	}

	localBody, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatalf("read local file after rm dry-run: %v", err)
	}
	if string(localBody) != "local changed\n" {
		t.Fatalf("local file was modified during rm --dry-run: %q", string(localBody))
	}

	if _, err := os.Stat(filepath.Join(cli.workDir, ".ob1", "base", "dry-rm.txt")); err != nil {
		t.Fatalf("merge base should remain after rm --dry-run, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(cli.workDir, ".ob1", "backup")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("backup directory should not be created during rm --dry-run, err=%v", err)
	}

	cacheAfter, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("read cache after rm dry-run: %v", err)
	}
	if string(cacheAfter) != string(cacheBefore) {
		t.Fatal("cache changed during rm --dry-run")
	}
}

func newCLIHarness(t *testing.T, apiBase string) *cliHarness {
	t.Helper()

	rootDir := t.TempDir()
	homeDir := filepath.Join(rootDir, "home")
	workDir := filepath.Join(rootDir, "vault")

	if err := os.Mkdir(homeDir, 0o755); err != nil {
		t.Fatalf("create home dir: %v", err)
	}
	if err := os.Mkdir(workDir, 0o755); err != nil {
		t.Fatalf("create work dir: %v", err)
	}

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	if err := os.Chdir(workDir); err != nil {
		t.Fatalf("change working directory: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(oldWD)
	})

	t.Setenv("HOME", homeDir)
	t.Setenv("OB1_API_BASE", apiBase)

	return &cliHarness{
		t:       t,
		homeDir: homeDir,
		workDir: workDir,
	}
}

func loginAndSetup(t *testing.T, cli *cliHarness, server *testserver.Server) {
	t.Helper()

	cli.run("login", "--email", server.Email(), "--password", server.AccountPassword()).requireSuccess(t)
	cli.run(
		"vault",
		"setup",
		server.VaultID(),
		"--vault-password",
		server.VaultPassword(),
		"--device-name",
		"test-device",
	).requireSuccess(t)
}

func requireDiff3(t *testing.T) {
	t.Helper()

	if _, err := exec.LookPath("diff3"); err != nil {
		t.Skip("diff3 is required for merge tests")
	}
}

func (h *cliHarness) run(args ...string) cliResult {
	h.t.Helper()

	captureDir := h.t.TempDir()
	stdoutFile, err := os.Create(filepath.Join(captureDir, "stdout.txt"))
	if err != nil {
		h.t.Fatalf("create stdout capture: %v", err)
	}
	stderrFile, err := os.Create(filepath.Join(captureDir, "stderr.txt"))
	if err != nil {
		_ = stdoutFile.Close()
		h.t.Fatalf("create stderr capture: %v", err)
	}

	oldStdout := os.Stdout
	oldStderr := os.Stderr
	os.Stdout = stdoutFile
	os.Stderr = stderrFile

	code := runWithArgs(args)

	os.Stdout = oldStdout
	os.Stderr = oldStderr

	if err := stdoutFile.Close(); err != nil {
		h.t.Fatalf("close stdout capture: %v", err)
	}
	if err := stderrFile.Close(); err != nil {
		h.t.Fatalf("close stderr capture: %v", err)
	}

	stdout, err := os.ReadFile(filepath.Join(captureDir, "stdout.txt"))
	if err != nil {
		h.t.Fatalf("read stdout capture: %v", err)
	}
	stderr, err := os.ReadFile(filepath.Join(captureDir, "stderr.txt"))
	if err != nil {
		h.t.Fatalf("read stderr capture: %v", err)
	}

	return cliResult{
		code:   code,
		stdout: string(stdout),
		stderr: string(stderr),
	}
}

func (r cliResult) requireSuccess(t *testing.T) {
	t.Helper()

	if r.code == 0 {
		return
	}

	t.Fatalf("command failed with exit code %d\nstdout:\n%s\nstderr:\n%s", r.code, r.stdout, r.stderr)
}

func (r cliResult) requireFailure(t *testing.T) {
	t.Helper()

	if r.code != 0 {
		return
	}

	t.Fatalf("command unexpectedly succeeded\nstdout:\n%s\nstderr:\n%s", r.stdout, r.stderr)
}
