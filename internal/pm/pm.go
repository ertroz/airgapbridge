package pm

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

type PackageManager string

const (
	Bun  PackageManager = "bun"
	Yarn PackageManager = "yarn"
	Pnpm PackageManager = "pnpm"
	Npm  PackageManager = "npm"
)

func IsValidPackageManager(v string) bool {
	switch PackageManager(v) {
	case Bun, Yarn, Pnpm, Npm:
		return true
	default:
		return false
	}
}

// DetectPackageManager checks lockfiles in priority order:
// 1. bun.lock / bun.lockb -> bun
// 2. yarn.lock -> yarn
// 3. pnpm-lock.yaml -> pnpm
// 4. package-lock.json (or fallback) -> npm
func DetectPackageManager(cwd string) PackageManager {
	if fileExists(filepath.Join(cwd, "bun.lock")) || fileExists(filepath.Join(cwd, "bun.lockb")) {
		return Bun
	}
	if fileExists(filepath.Join(cwd, "yarn.lock")) {
		return Yarn
	}
	if fileExists(filepath.Join(cwd, "pnpm-lock.yaml")) {
		return Pnpm
	}
	if fileExists(filepath.Join(cwd, "package-lock.json")) {
		return Npm
	}
	return Npm
}

func DefaultScriptCommand(pm PackageManager, script string) string {
	switch pm {
	case Bun:
		return "bun run " + script
	case Yarn:
		return "yarn " + script
	case Pnpm:
		return "pnpm " + script
	default:
		return "npm run " + script
	}
}

func DefaultDevCommand(pm PackageManager) string {
	return DefaultScriptCommand(pm, "dev")
}

func DefaultBuildCommand(pm PackageManager) string {
	return DefaultScriptCommand(pm, "build")
}

func DefaultStartCommand(pm PackageManager) string {
	return DefaultScriptCommand(pm, "start")
}

func DefaultPreviewCommand(pm PackageManager) string {
	return DefaultScriptCommand(pm, "preview")
}

func ResolveDevCommand(cwd, pmOverride, configCommand string) (string, error) {
	return ResolveScriptCommand(cwd, pmOverride, "", configCommand, DefaultDevCommand)
}

func ResolveScriptCommand(cwd, pmOverride, flagCommand, configCommand string, defaultFn func(PackageManager) string) (string, error) {
	if strings.TrimSpace(flagCommand) != "" {
		return flagCommand, nil
	}
	if pmOverride != "" {
		if !IsValidPackageManager(pmOverride) {
			return "", fmt.Errorf("invalid package manager %q (supported: bun, npm, yarn, pnpm)", pmOverride)
		}
		return defaultFn(PackageManager(pmOverride)), nil
	}
	if strings.TrimSpace(configCommand) != "" {
		return configCommand, nil
	}
	return defaultFn(DetectPackageManager(cwd)), nil
}

func AppendPortFlagIfDefaultPreview(command string, port int) string {
	if port <= 0 {
		return command
	}
	trimmed := strings.TrimSpace(command)
	if strings.Contains(trimmed, "--port") || strings.Contains(trimmed, " -p ") {
		return command
	}
	switch trimmed {
	case "npm run preview":
		return fmt.Sprintf("npm run preview -- --port %d", port)
	case "pnpm preview", "yarn preview", "bun run preview":
		return fmt.Sprintf("%s --port %d", trimmed, port)
	default:
		return command
	}
}

func IsPortListening(port int) bool {
	addrs := []string{
		fmt.Sprintf("127.0.0.1:%d", port),
		fmt.Sprintf("[::1]:%d", port),
	}
	for _, addr := range addrs {
		conn, err := net.DialTimeout("tcp", addr, 350*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return true
		}
	}
	return false
}

func WaitForPort(port int, timeout time.Duration, exitCh <-chan error) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if IsPortListening(port) {
			return nil
		}
		select {
		case err := <-exitCh:
			if err != nil {
				return fmt.Errorf("server exited prematurely: %w", err)
			}
			return errors.New("server exited prematurely before port became ready")
		case <-time.After(250 * time.Millisecond):
		}
	}
	return fmt.Errorf("timed out waiting for server on port %d", port)
}

func RunBuild(ctx context.Context, command, cwd string, interactive bool) error {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/C", command)
	} else {
		cmd = exec.Command("sh", "-c", command)
	}
	cmd.Dir = cwd
	if !interactive {
		configureProcessGroup(cmd)
	}

	if interactive {
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		if err := cmd.Start(); err != nil {
			return err
		}

		exitCh := make(chan error, 1)
		go func() {
			exitCh <- cmd.Wait()
		}()

		select {
		case <-ctx.Done():
			KillProcessTree(cmd)
			<-exitCh
			return ctx.Err()
		case err := <-exitCh:
			return err
		}
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		return err
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		pipePrefixed(stdout, os.Stdout, "[build]")
	}()
	go func() {
		defer wg.Done()
		pipePrefixed(stderr, os.Stderr, "[build]")
	}()

	exitCh := make(chan error, 1)
	go func() {
		wg.Wait()
		exitCh <- cmd.Wait()
	}()

	select {
	case <-ctx.Done():
		KillProcessTree(cmd)
		<-exitCh
		return ctx.Err()
	case err := <-exitCh:
		return err
	}
}

func SpawnDevServer(command, cwd string) (*exec.Cmd, <-chan error, error) {
	return SpawnDevServerWithOptions(command, cwd, false)
}

func SpawnDevServerWithOptions(command, cwd string, interactive bool) (*exec.Cmd, <-chan error, error) {
	return SpawnServerWithOptions(command, cwd, "[dev]", interactive)
}

func SpawnServerWithOptions(command, cwd, logPrefix string, interactive bool, extraEnv ...string) (*exec.Cmd, <-chan error, error) {
	if logPrefix == "" {
		logPrefix = "[dev]"
	}

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/C", command)
	} else {
		cmd = exec.Command("sh", "-c", command)
	}
	cmd.Dir = cwd
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	if !interactive {
		configureProcessGroup(cmd)
	}

	if interactive {
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		if err := cmd.Start(); err != nil {
			return nil, nil, err
		}

		exitCh := make(chan error, 1)
		go func() {
			exitCh <- cmd.Wait()
		}()

		return cmd, exitCh, nil
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, err
	}

	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}

	go pipePrefixed(stdout, os.Stdout, logPrefix)
	go pipePrefixed(stderr, os.Stderr, logPrefix)

	exitCh := make(chan error, 1)
	go func() {
		exitCh <- cmd.Wait()
	}()

	return cmd, exitCh, nil
}

func pipePrefixed(r io.Reader, w io.Writer, prefix string) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) != "" {
			_, _ = fmt.Fprintf(w, "%s %s\n", prefix, line)
		}
	}
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}
