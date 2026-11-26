package pm

import (
	"context"
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

func TestDefaultBuildStartPreviewCommands(t *testing.T) {
	if got := DefaultBuildCommand(Bun); got != "bun run build" {
		t.Fatalf("unexpected bun build cmd: %q", got)
	}
	if got := DefaultStartCommand(Yarn); got != "yarn start" {
		t.Fatalf("unexpected yarn start cmd: %q", got)
	}
	if got := DefaultPreviewCommand(Pnpm); got != "pnpm preview" {
		t.Fatalf("unexpected pnpm preview cmd: %q", got)
	}
	if got := DefaultPreviewCommand(Npm); got != "npm run preview" {
		t.Fatalf("unexpected npm preview cmd: %q", got)
	}
}

func TestResolveScriptCommandAndPreviewPortFlag(t *testing.T) {
	dir := t.TempDir()
	writeEmpty(t, filepath.Join(dir, "pnpm-lock.yaml"))

	// Flag command overrides everything
	cmd, err := ResolveScriptCommand(dir, "bun", "vite preview --port 9999", "pnpm preview", DefaultPreviewCommand)
	if err != nil || cmd != "vite preview --port 9999" {
		t.Fatalf("expected flag override, got %q, err=%v", cmd, err)
	}

	// PM override overrides config
	cmd, err = ResolveScriptCommand(dir, "npm", "", "pnpm preview", DefaultPreviewCommand)
	if err != nil || cmd != "npm run preview" {
		t.Fatalf("expected npm override, got %q, err=%v", cmd, err)
	}

	if got := AppendPortFlagIfDefaultPreview("npm run preview", 4175); got != "npm run preview -- --port 4175" {
		t.Fatalf("unexpected npm port flag cmd: %q", got)
	}
	if got := AppendPortFlagIfDefaultPreview("pnpm preview", 4175); got != "pnpm preview --port 4175" {
		t.Fatalf("unexpected pnpm port flag cmd: %q", got)
	}
	if got := AppendPortFlagIfDefaultPreview("npx serve dist", 4175); got != "npx serve dist" {
		t.Fatalf("custom command should not be modified, got %q", got)
	}
}

func TestRunBuild(t *testing.T) {
	dir := t.TempDir()
	if err := RunBuild(context.Background(), "echo built", dir, false); err != nil {
		t.Fatalf("RunBuild failed: %v", err)
	}
	if err := RunBuild(context.Background(), "exit 2", dir, false); err == nil {
		t.Fatal("expected error from failing build command")
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
