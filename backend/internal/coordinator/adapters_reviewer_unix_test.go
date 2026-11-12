//go:build !windows

package coordinator

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestCodexReviewerRecordsTheNativeThreadIDAndVerdict(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-codex")
	body := `#!/bin/sh
out=""
while [ $# -gt 0 ]; do
  if [ "$1" = "-o" ]; then out="$2"; shift 2; else shift; fi
done
printf '%s\n' '{"type":"thread.started","thread_id":"019f6af0-codex-session"}'
printf 'PASS\nReviewed the exact commit.\n' > "$out"
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	reviewer := CodexReviewer{Binary: script, Model: "gpt-6.1-sol", Effort: "low"}
	result, err := reviewer.Review(context.Background(), ReviewRequest{Issue: "CKM-1", Commit: commitOne, Worktree: dir, Acceptance: "must work"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Session != "019f6af0-codex-session" {
		t.Fatalf("reviewer session = %q", result.Session)
	}
	if result.Verdict != "pass" || result.Source != "codex-cli" {
		t.Fatalf("review result = %+v", result)
	}
}

func TestCodexReviewerRefusesToInventASessionIdentity(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-codex")
	body := `#!/bin/sh
out=""
while [ $# -gt 0 ]; do
  if [ "$1" = "-o" ]; then out="$2"; shift 2; else shift; fi
done
printf '%s\n' '{"type":"turn.completed"}'
printf 'PASS\n' > "$out"
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	reviewer := CodexReviewer{Binary: script}
	if _, err := reviewer.Review(context.Background(), ReviewRequest{Issue: "CKM-1", Commit: commitOne, Worktree: dir}); err == nil {
		t.Fatal("reviewer without a native session id was recorded")
	}
}
