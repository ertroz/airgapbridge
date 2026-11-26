package browser

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"airgapbridge/internal/pm"
)

type Instance struct {
	Cmd            *exec.Cmd
	ExitCh         <-chan error
	ProfileDir     string
	ExecutablePath string
}

func (b *Instance) Close() {
	if b == nil {
		return
	}
	pm.KillProcessTree(b.Cmd)
	removeProfileDir(b.ProfileDir)
}

func removeProfileDir(dir string) {
	if dir == "" {
		return
	}
	for i := 0; i < 4; i++ {
		if err := os.RemoveAll(dir); err == nil {
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
}

func FindExecutable(explicitPath string) (string, error) {
	override := explicitPath
	if override == "" {
		override = os.Getenv("AIRGAPBRIDGE_BROWSER")
	}
	if override != "" {
		if info, err := os.Stat(override); err == nil && !info.IsDir() {
			return override, nil
		}
		return "", fmt.Errorf("specified browser executable not found: %q (pass --browser <path> or set AIRGAPBRIDGE_BROWSER)", override)
	}

	for _, candidate := range candidatePaths() {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}

	for _, bin := range []string{"google-chrome", "google-chrome-stable", "microsoft-edge", "msedge", "chromium", "chromium-browser"} {
		if p, err := exec.LookPath(bin); err == nil {
			return p, nil
		}
	}

	return "", fmt.Errorf("could not find Google Chrome or Microsoft Edge on %s; install Chrome/Edge or pass --browser <path>", runtime.GOOS)
}

func candidatePaths() []string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		return []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			filepath.Join(home, "Applications/Google Chrome.app/Contents/MacOS/Google Chrome"),
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
			filepath.Join(home, "Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge"),
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
		}
	case "windows":
		pf := os.Getenv("PROGRAMFILES")
		if pf == "" {
			pf = `C:\Program Files`
		}
		pfx86 := os.Getenv("PROGRAMFILES(X86)")
		if pfx86 == "" {
			pfx86 = `C:\Program Files (x86)`
		}
		localApp := os.Getenv("LOCALAPPDATA")
		if localApp == "" {
			localApp = filepath.Join(home, "AppData", "Local")
		}
		return []string{
			filepath.Join(pf, "Google", "Chrome", "Application", "chrome.exe"),
			filepath.Join(pfx86, "Google", "Chrome", "Application", "chrome.exe"),
			filepath.Join(localApp, "Google", "Chrome", "Application", "chrome.exe"),
			filepath.Join(pf, "Microsoft", "Edge", "Application", "msedge.exe"),
			filepath.Join(pfx86, "Microsoft", "Edge", "Application", "msedge.exe"),
			filepath.Join(localApp, "Microsoft", "Edge", "Application", "msedge.exe"),
		}
	default:
		return []string{
			"/usr/bin/google-chrome",
			"/usr/bin/google-chrome-stable",
			"/usr/bin/microsoft-edge",
			"/usr/bin/chromium",
			"/usr/bin/chromium-browser",
			"/snap/bin/chromium",
		}
	}
}

func LaunchIsolated(sitURL string, proxyPort int, explicitPath string) (*Instance, error) {
	exe, err := FindExecutable(explicitPath)
	if err != nil {
		return nil, err
	}

	profileDir, err := os.MkdirTemp("", "airgapbridge-profile-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp browser profile dir: %w", err)
	}

	args := []string{
		fmt.Sprintf("--user-data-dir=%s", profileDir),
		fmt.Sprintf("--proxy-server=http://127.0.0.1:%d", proxyPort),
		"--ignore-certificate-errors",
		"--no-first-run",
		"--no-default-browser-check",
		sitURL,
	}

	cmd := exec.Command(exe, args...)
	if err := cmd.Start(); err != nil {
		removeProfileDir(profileDir)
		return nil, fmt.Errorf("failed to launch browser %q: %w", exe, err)
	}

	exitCh := make(chan error, 1)
	go func() {
		exitCh <- cmd.Wait()
	}()

	return &Instance{
		Cmd:            cmd,
		ExitCh:         exitCh,
		ProfileDir:     profileDir,
		ExecutablePath: exe,
	}, nil
}
