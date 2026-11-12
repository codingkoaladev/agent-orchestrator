package coordinator

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestRunLockHelperProcess is not a real test. It is the child process used by
// the cross-process lock test below; it holds the lock until killed.
func TestRunLockHelperProcess(t *testing.T) {
	dir := os.Getenv("GO_RUN_LOCK_DIR")
	if dir == "" {
		t.Skip("helper process for TestStoreLockIsExclusiveAcrossProcesses")
	}
	store := Store{Dir: dir}
	lock, err := store.Lock(context.Background(), "CKM-825")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	fmt.Println("locked")
	time.Sleep(30 * time.Second)
}

func TestStoreLockIsExclusiveAcrossProcesses(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=TestRunLockHelperProcess")
	cmd.Env = append(os.Environ(), "GO_RUN_LOCK_DIR="+dir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() || scanner.Text() != "locked" {
		t.Fatalf("helper did not take the lock: %q %v", scanner.Text(), scanner.Err())
	}

	store := Store{Dir: dir}
	if _, err := store.Lock(context.Background(), "CKM-825"); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second process lock = %v, want ErrAlreadyRunning", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_, _ = cmd.Process.Wait()

	deadline := time.Now().Add(2 * time.Second)
	for {
		lock, err := store.Lock(context.Background(), "CKM-825")
		if err == nil {
			_ = lock.Release()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("lock was not released after the owner died: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
