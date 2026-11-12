//go:build !windows

package coordinator

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitRepo(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	runGit := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	runGit("init", "-b", "main")
	runGit("config", "user.email", "test@example.com")
	runGit("config", "user.name", "Test")
	runGit("config", "core.autocrlf", "false")
	if err := os.WriteFile(filepath.Join(dir, "code.txt"), []byte("committed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "code.txt")
	runGit("commit", "-m", "initial")
	return dir, runGit("rev-parse", "HEAD")
}

func TestGitCheckoutAllowsUnstagedEnvironmentFilesButRejectsStagedCode(t *testing.T) {
	dir, head := gitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "code.txt"), []byte("local environment\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	state, err := (GitCheckout{}).Validate(context.Background(), dir, "main", head)
	if err != nil {
		t.Fatalf("unstaged local change blocked the submission: %v", err)
	}
	if len(state.Unstaged) != 1 || state.Head != head || state.Branch != "main" {
		t.Fatalf("checkout state = %#v", state)
	}
	if _, err := exec.Command("git", "-C", dir, "add", "code.txt").CombinedOutput(); err != nil {
		t.Fatal(err)
	}
	if _, err := (GitCheckout{}).Validate(context.Background(), dir, "main", head); err == nil {
		t.Fatal("staged code outside the submitted commit was accepted")
	}
}

func TestGitCheckoutRejectsTheWrongCommitOrBranch(t *testing.T) {
	dir, head := gitRepo(t)
	if _, err := (GitCheckout{}).Validate(context.Background(), dir, "main", strings.Repeat("a", 40)); err == nil {
		t.Fatal("wrong submitted commit was accepted")
	}
	if _, err := (GitCheckout{}).Validate(context.Background(), dir, "other", head); err == nil {
		t.Fatal("wrong intended branch was accepted")
	}
}

func TestGitSnapshotContainsOnlyTheCommittedTree(t *testing.T) {
	dir, head := gitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "code.txt"), []byte("dirty environment\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshot, cleanup, err := (GitSnapshot{}).Create(context.Background(), dir, head)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	data, err := os.ReadFile(filepath.Join(snapshot, "code.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "committed\n" {
		t.Fatalf("snapshot leaked uncommitted content: %q", data)
	}
	headOut, err := exec.Command("git", "-C", snapshot, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("snapshot has no commit history: %v", err)
	}
	if strings.TrimSpace(string(headOut)) != head {
		t.Fatalf("snapshot HEAD = %q, want %q", headOut, head)
	}
	cleanup()
	if _, err := os.Stat(snapshot); !os.IsNotExist(err) {
		t.Fatalf("snapshot cleanup left %s", snapshot)
	}
}

func TestGHPullRequestsRequiresExactlyOneOpenPR(t *testing.T) {
	for _, tc := range []struct {
		name    string
		output  string
		wantErr bool
	}{
		{"none", `[]`, true},
		{"one", `[{"number":857,"headRefName":"CKM-830","headRefOid":"cd5a1628cddbb58acc31bc75ecea27070a1ad107","baseRefName":"prelive","state":"OPEN"}]`, false},
		{"ambiguous", `[{"number":1,"headRefName":"b"},{"number":2,"headRefName":"b"}]`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			script := filepath.Join(dir, "fake-gh")
			body := "#!/bin/sh\nprintf '%s' '" + strings.ReplaceAll(tc.output, "'", "'\\''") + "'\n"
			if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
			_, err := (GHPullRequests{Binary: script}).Find(context.Background(), dir, "CKM-830")
			if tc.wantErr && err == nil {
				t.Fatal("ambiguous or absent PR was accepted")
			}
			if !tc.wantErr && err != nil {
				t.Fatal(err)
			}
		})
	}
}
