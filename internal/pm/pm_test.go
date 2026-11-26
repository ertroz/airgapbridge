package pm

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectPackageManagerPriority(t *testing.T) {
	dir := t.TempDir()

	if got := DetectPackageManager(dir); got != Npm {
		t.Fatalf("expected fallback npm, got %s", got)
	}

	writeEmpty(t, filepath.Join(dir, "package-lock.json"))
	if got := DetectPackageManager(dir); got != Npm {
		t.Fatalf("expected npm, got %s", got)
	}

	writeEmpty(t, filepath.Join(dir, "pnpm-lock.yaml"))
	if got := DetectPackageManager(dir); got != Pnpm {
		t.Fatalf("expected pnpm, got %s", got)
	}

	writeEmpty(t, filepath.Join(dir, "yarn.lock"))
	if got := DetectPackageManager(dir); got != Yarn {
		t.Fatalf("expected yarn, got %s", got)
	}

	writeEmpty(t, filepath.Join(dir, "bun.lockb"))
	if got := DetectPackageManager(dir); got != Bun {
		t.Fatalf("expected bun (bun.lockb), got %s", got)
	}

	_ = os.Remove(filepath.Join(dir, "bun.lockb"))
	writeEmpty(t, filepath.Join(dir, "bun.lock"))
	if got := DetectPackageManager(dir); got != Bun {
		t.Fatalf("expected bun (bun.lock), got %s", got)
	}
}

func TestResolveDevCommand(t *testing.T) {
	dir := t.TempDir()

	cmd, err := ResolveDevCommand(dir, "pnpm", "")
	if err != nil || cmd != "pnpm dev" {
		t.Fatalf("expected 'pnpm dev', got %q, err=%v", cmd, err)
	}

	cmd, err = ResolveDevCommand(dir, "", "custom-dev --port 4000")
	if err != nil || cmd != "custom-dev --port 4000" {
		t.Fatalf("expected custom command, got %q, err=%v", cmd, err)
	}

	if _, err := ResolveDevCommand(dir, "invalid", ""); err == nil {
		t.Fatal("expected error for invalid package manager")
	}
}

func TestSpawnDevServerWithOptions(t *testing.T) {
	dir := t.TempDir()
	cmd, exitCh, err := SpawnDevServerWithOptions("echo ok", dir, false)
	if err != nil {
		t.Fatalf("SpawnDevServerWithOptions failed: %v", err)
	}
	if cmd == nil {
		t.Fatal("expected non-nil cmd")
	}
	if err := <-exitCh; err != nil {
		t.Fatalf("expected clean exit, got %v", err)
	}
}

func writeEmpty(t *testing.T, p string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
}
