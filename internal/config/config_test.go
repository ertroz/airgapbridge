package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectFramework(t *testing.T) {
	nextDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(nextDir, "package.json"), []byte(`{"dependencies":{"next":"15.0.0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	fw, port := DetectFramework(nextDir)
	if fw != FrameworkNextJS || port != 3000 {
		t.Fatalf("expected nextjs:3000, got %s:%d", fw, port)
	}

	viteDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(viteDir, "package.json"), []byte(`{"devDependencies":{"vite":"6.0.0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	fw, port = DetectFramework(viteDir)
	if fw != FrameworkVite || port != 5173 {
		t.Fatalf("expected vite:5173, got %s:%d", fw, port)
	}
}

func TestRunInitWizardAndLoad(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pnpm-lock.yaml"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"devDependencies":{"vite":"6.0.0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := RunInitWizard(dir, InitOptions{
		SitURL: "https://sit-app.company.com/",
		Yes:    true,
	})
	if err != nil {
		t.Fatalf("RunInitWizard failed: %v", err)
	}
	if cfg.SitURL != "https://sit-app.company.com" || cfg.LocalPort != 5173 || cfg.DevCommand != "pnpm dev" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	if cfg.QAPort != 4173 || cfg.BuildCommand != "pnpm build" || cfg.QACommand != "pnpm preview" {
		t.Fatalf("unexpected QA build config: %+v", cfg)
	}

	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if loaded.SitURL != cfg.SitURL || loaded.LocalPort != cfg.LocalPort || loaded.ProxyPort != DefaultProxyPort || loaded.QAPort != 4173 {
		t.Fatalf("unexpected loaded config: %+v", loaded)
	}
}

func TestLoadDefaultsForLegacyConfig(t *testing.T) {
	nextDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(nextDir, "bun.lock"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nextDir, "package.json"), []byte(`{"dependencies":{"next":"15.0.0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	legacy := `{"sitUrl":"https://sit-app.company.com","localPort":3000,"devCommand":"bun run dev"}`
	if err := os.WriteFile(filepath.Join(nextDir, ConfigFilename), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(nextDir)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.QAPort != 3000 || cfg.BuildCommand != "bun run build" || cfg.QACommand != "bun run start" {
		t.Fatalf("unexpected legacy Next.js QA defaults: %+v", cfg)
	}
}

func TestLoadInteractiveDev(t *testing.T) {
	dir := t.TempDir()
	content := `{"sitUrl":"https://sit-app.company.com","localPort":3000,"interactiveDev":true}`
	if err := os.WriteFile(filepath.Join(dir, ConfigFilename), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if !cfg.InteractiveDev {
		t.Fatal("expected InteractiveDev to be true")
	}
}
