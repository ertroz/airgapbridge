package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"airgapbridge/internal/browser"
	"airgapbridge/internal/config"
	"airgapbridge/internal/pm"
	"airgapbridge/internal/proxy"
	"airgapbridge/internal/tunnel"
)

const version = "1.2.0"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(0)
	}

	subcmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch subcmd {
	case "init":
		err = runInit(args)
	case "dev":
		err = runDev(args)
	case "qa":
		err = runQA(args)
	case "qa-dev":
		err = runQADev(args)
	case "version", "-v", "--version":
		fmt.Printf("airgapbridge v%s\n", version)
		return
	case "help", "-h", "--help":
		printUsage()
		return
	default:
		fmt.Fprintf(os.Stderr, "Unknown command %q\n\n", subcmd)
		printUsage()
		os.Exit(1)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "\n[airgapbridge] Error: %v\n\n", err)
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Printf(`airgapbridge v%s — Zero-token local dev & QA bridge for Next.js/Vite behind Entra ID SSO & corporate VPN

Usage:
  airgapbridge <command> [flags]

Commands:
  init       Generate airgapbridge.config.json in the current project
  dev        Start local dev server, selective TLS proxy, and isolated browser for SSO dev
  qa         Build production bundle, serve build output via cloudflared, and generate QA rules
  qa-dev     Expose local dev server via cloudflared and generate QA rules
  version    Print CLI version
  help       Show this help message

Run "airgapbridge <command> --help" for command-specific flags.
`, version)
}

func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	sitURL := fs.String("sit-url", "", "Target SIT frontend URL (e.g. https://sit-app.company.com)")
	port := fs.Int("port", 0, "Local dev server port")
	qaPort := fs.Int("qa-port", 0, "QA build server port")
	pmFlag := fs.String("pm", "", "Package manager override (bun | npm | yarn | pnpm)")
	devCmd := fs.String("dev-command", "", "Custom local dev command")
	buildCmd := fs.String("build-command", "", "Custom QA build command")
	qaCmd := fs.String("qa-command", "", "Custom QA serve/preview command")
	bypass := fs.String("bypass", "", "Comma-separated regex patterns to bypass to real SIT")
	yes := fs.Bool("yes", false, "Skip interactive prompts using defaults/flags")
	fs.BoolVar(yes, "y", false, "Skip interactive prompts using defaults/flags")

	if err := fs.Parse(args); err != nil {
		return err
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	cfg, err := config.RunInitWizard(cwd, config.InitOptions{
		SitURL:       *sitURL,
		Port:         *port,
		QAPort:       *qaPort,
		PM:           *pmFlag,
		DevCommand:   *devCmd,
		BuildCommand: *buildCmd,
		QACommand:    *qaCmd,
		Bypass:       *bypass,
		Yes:          *yes,
	})
	if err != nil {
		return err
	}

	pretty, _ := json.MarshalIndent(cfg, "", "  ")
	fmt.Printf("\n[airgapbridge] Created %s:\n%s\n", config.ConfigFilename, string(pretty))
	config.CheckFrameworkCompatibility(cwd, cfg.SitURL)
	fmt.Println("\nNext steps:")
	fmt.Println("  1. Run \"airgapbridge dev\"    for local SSO development")
	fmt.Println("  2. Run \"airgapbridge qa\"     to build & share a production build with QA")
	fmt.Println("  3. Run \"airgapbridge qa-dev\" to share the live dev server with QA")
	return nil
}

func runDev(args []string) error {
	fs := flag.NewFlagSet("dev", flag.ContinueOnError)
	pmFlag := fs.String("pm", "", "Package manager override (bun | npm | yarn | pnpm)")
	portFlag := fs.Int("port", 0, "Override local dev server port")
	proxyPortFlag := fs.Int("proxy-port", 0, "Override local proxy port")
	browserFlag := fs.String("browser", "", "Path to Chrome or Edge executable")
	noBrowser := fs.Bool("no-browser", false, "Start proxy and dev server without launching a browser")
	interactiveFlag := fs.Bool("interactive", false, "Attach terminal stdin/stdout to dev server (for password prompts like airgapvault)")
	fs.BoolVar(interactiveFlag, "i", false, "Attach terminal stdin/stdout to dev server")

	if err := fs.Parse(args); err != nil {
		return err
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	cfg, err := config.Load(cwd)
	if err != nil {
		return err
	}

	localPort := cfg.LocalPort
	if *portFlag > 0 {
		localPort = *portFlag
	}
	proxyPort := cfg.ProxyPort
	if *proxyPortFlag > 0 {
		proxyPort = *proxyPortFlag
	}
	interactive := cfg.InteractiveDev || *interactiveFlag

	config.CheckFrameworkCompatibility(cwd, cfg.SitURL)

	var devCmd *exec.Cmd
	var proxySrv *proxy.Server
	var browserInst *browser.Instance

	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			if browserInst != nil {
				browserInst.Close()
			}
			if proxySrv != nil {
				_ = proxySrv.Close()
			}
			if devCmd != nil {
				pm.KillProcessTree(devCmd)
			}
		})
	}
	defer cleanup()

	devCmd, _, err = ensureDevServerRunning(cwd, localPort, cfg.DevCommand, *pmFlag, interactive)
	if err != nil {
		return err
	}

	proxySrv, err = proxy.Start(proxy.Options{
		SitURL:      cfg.SitURL,
		LocalPort:   localPort,
		ProxyPort:   proxyPort,
		BypassPaths: cfg.BypassPaths,
	})
	if err != nil {
		return err
	}

	sitParsed, _ := url.Parse(cfg.SitURL)
	fmt.Printf("[airgapbridge] Proxy listening on http://127.0.0.1:%d\n", proxySrv.ProxyPort)
	fmt.Printf("[airgapbridge] Intercepting ONLY %s -> http://127.0.0.1:%d\n", sitParsed.Host, localPort)
	if len(cfg.BypassPaths) > 0 {
		fmt.Printf("[airgapbridge] Bypassing to real SIT: %v\n", cfg.BypassPaths)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	if !*noBrowser {
		browserInst, err = browser.LaunchIsolated(cfg.SitURL, proxySrv.ProxyPort, *browserFlag)
		if err != nil {
			return err
		}
		fmt.Printf("[airgapbridge] Launched isolated browser (%s)\n", filepath.Base(browserInst.ExecutablePath))
		fmt.Printf("[airgapbridge] Opening %s (Press Ctrl+C to stop)...\n", cfg.SitURL)

		select {
		case sig := <-sigCh:
			fmt.Printf("\n[airgapbridge] Received %s, shutting down cleanly...\n", sig)
		case <-browserInst.ExitCh:
			fmt.Println("\n[airgapbridge] Browser closed. Cleaning up...")
		}
	} else {
		fmt.Printf("[airgapbridge] Running in headless proxy mode. Set browser proxy to 127.0.0.1:%d. Press Ctrl+C to stop.\n", proxySrv.ProxyPort)
		sig := <-sigCh
		fmt.Printf("\n[airgapbridge] Received %s, shutting down cleanly...\n", sig)
	}

	cleanup()
	return nil
}

func runQA(args []string) error {
	fs := flag.NewFlagSet("qa", flag.ContinueOnError)
	pmFlag := fs.String("pm", "", "Package manager override (bun | npm | yarn | pnpm)")
	portFlag := fs.Int("port", 0, "Override QA build server port")
	buildCmdFlag := fs.String("build-command", "", "Override QA build command")
	qaCmdFlag := fs.String("qa-command", "", "Override QA serve/preview command")
	skipBuild := fs.Bool("skip-build", false, "Skip running build and serve existing build output")
	outputFile := fs.String("output", tunnel.QARulesFilename, "Output QA rules filename")
	fs.StringVar(outputFile, "o", tunnel.QARulesFilename, "Output QA rules filename")
	interactiveFlag := fs.Bool("interactive", false, "Attach terminal stdin/stdout to build & QA server (for password prompts like airgapvault)")
	fs.BoolVar(interactiveFlag, "i", false, "Attach terminal stdin/stdout to build & QA server")
	protocolFlag := fs.String("protocol", "http2", "Cloudflared transport protocol (http2 | quic | auto)")
	formatFlag := fs.String("format", "header-editor", "QA rules output format (header-editor | requestly)")

	if err := fs.Parse(args); err != nil {
		return err
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	cfg, err := config.Load(cwd)
	if err != nil {
		return err
	}

	fw, _ := config.DetectFramework(cwd)
	qaPort := cfg.QAPort
	if *portFlag > 0 {
		qaPort = *portFlag
	}
	interactive := cfg.InteractiveDev || *interactiveFlag

	buildCmd, err := pm.ResolveScriptCommand(cwd, *pmFlag, *buildCmdFlag, cfg.BuildCommand, pm.DefaultBuildCommand)
	if err != nil {
		return err
	}

	qaCmd, err := pm.ResolveScriptCommand(cwd, *pmFlag, *qaCmdFlag, cfg.QACommand, func(p pm.PackageManager) string {
		return config.DefaultQACommand(p, fw)
	})
	if err != nil {
		return err
	}
	if fw == config.FrameworkVite && qaPort != config.DefaultViteQAPort {
		qaCmd = pm.AppendPortFlagIfDefaultPreview(qaCmd, qaPort)
	}

	config.CheckQABuildCompatibility(cwd, cfg.SitURL)

	var srvCmd *exec.Cmd
	var srvExitCh <-chan error
	var tunInst *tunnel.Instance

	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			if tunInst != nil {
				tunInst.Close()
			}
			if srvCmd != nil {
				pm.KillProcessTree(srvCmd)
			}
		})
	}
	defer cleanup()

	if !*skipBuild {
		if pm.IsPortListening(qaPort) {
			return fmt.Errorf(
				"port %d is already in use; stop the running server (e.g. \"airgapbridge dev\") or pass --port <port> / --skip-build",
				qaPort,
			)
		}

		fmt.Printf("[airgapbridge] Building production bundle (%s)...\n", buildCmd)
		buildCtx, stopBuildSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		buildErr := pm.RunBuild(buildCtx, buildCmd, cwd, interactive)
		stopBuildSignals()
		if buildErr != nil {
			if errors.Is(buildCtx.Err(), context.Canceled) {
				fmt.Println("\n[airgapbridge] Build interrupted, shutting down cleanly...")
				return nil
			}
			return fmt.Errorf("build failed: %w", buildErr)
		}
		fmt.Println("[airgapbridge] Build completed.")
	}

	srvCmd, srvExitCh, err = ensureQAServerRunning(cwd, qaPort, qaCmd, *skipBuild, interactive)
	if err != nil {
		return err
	}

	return startQATunnelAndWait(cwd, cfg, qaPort, *protocolFlag, *formatFlag, *outputFile, "Build", &tunInst, srvExitCh, cleanup)
}

func runQADev(args []string) error {
	fs := flag.NewFlagSet("qa-dev", flag.ContinueOnError)
	pmFlag := fs.String("pm", "", "Package manager override (bun | npm | yarn | pnpm)")
	portFlag := fs.Int("port", 0, "Override local dev server port")
	outputFile := fs.String("output", tunnel.QARulesFilename, "Output QA rules filename")
	fs.StringVar(outputFile, "o", tunnel.QARulesFilename, "Output QA rules filename")
	interactiveFlag := fs.Bool("interactive", false, "Attach terminal stdin/stdout to dev server (for password prompts like airgapvault)")
	fs.BoolVar(interactiveFlag, "i", false, "Attach terminal stdin/stdout to dev server")
	protocolFlag := fs.String("protocol", "http2", "Cloudflared transport protocol (http2 | quic | auto)")
	formatFlag := fs.String("format", "header-editor", "QA rules output format (header-editor | requestly)")

	if err := fs.Parse(args); err != nil {
		return err
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	cfg, err := config.Load(cwd)
	if err != nil {
		return err
	}

	localPort := cfg.LocalPort
	if *portFlag > 0 {
		localPort = *portFlag
	}
	interactive := cfg.InteractiveDev || *interactiveFlag

	config.CheckFrameworkCompatibility(cwd, cfg.SitURL)

	var devCmd *exec.Cmd
	var devExitCh <-chan error
	var tunInst *tunnel.Instance

	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			if tunInst != nil {
				tunInst.Close()
			}
			if devCmd != nil {
				pm.KillProcessTree(devCmd)
			}
		})
	}
	defer cleanup()

	devCmd, devExitCh, err = ensureDevServerRunning(cwd, localPort, cfg.DevCommand, *pmFlag, interactive)
	if err != nil {
		return err
	}

	return startQATunnelAndWait(cwd, cfg, localPort, *protocolFlag, *formatFlag, *outputFile, "Dev Server", &tunInst, devExitCh, cleanup)
}

func startQATunnelAndWait(
	cwd string,
	cfg *config.Config,
	targetPort int,
	protocol string,
	format string,
	outputFile string,
	modeLabel string,
	tunInstOut **tunnel.Instance,
	serverExitCh <-chan error,
	cleanup func(),
) error {
	fmt.Printf("[airgapbridge] Starting cloudflared tunnel (%s) to http://localhost:%d...\n", protocol, targetPort)
	tunInst, err := tunnel.StartCloudflaredTunnel(targetPort, 30*time.Second, protocol)
	if err != nil {
		return err
	}
	*tunInstOut = tunInst

	var rulesPath string
	switch format {
	case "header-editor":
		rulesPath, err = tunnel.WriteHeaderEditorRulesFile(cwd, cfg.SitURL, tunInst.URL, cfg.BypassPaths, outputFile)
	case "requestly":
		rulesPath, err = tunnel.WriteRequestlyRulesFile(cwd, cfg.SitURL, tunInst.URL, cfg.BypassPaths, outputFile)
	default:
		return fmt.Errorf("invalid --format %q (supported: header-editor, requestly)", format)
	}
	if err != nil {
		return err
	}

	copied := tunnel.CopyToClipboard(tunInst.URL)
	copiedSuffix := ""
	if copied {
		copiedSuffix = " (copied to clipboard!)"
	}

	extName := "Header Editor"
	if format == "requestly" {
		extName = "Requestly"
	}

	fmt.Println("\n============================================================")
	fmt.Printf("  AirgapBridge QA Tunnel Ready (%s)\n", modeLabel)
	fmt.Println("============================================================")
	fmt.Printf("  Local URL:   http://localhost:%d\n", targetPort)
	fmt.Printf("  Tunnel URL:  %s%s\n", tunInst.URL, copiedSuffix)
	fmt.Printf("  SIT Domain:  %s\n", cfg.SitURL)
	fmt.Printf("  Rules File:  %s (%s)\n", filepath.Base(rulesPath), extName)
	fmt.Println("")
	fmt.Println("  Share with QA:")
	fmt.Printf("  1. Send %q to your QA engineer.\n", filepath.Base(rulesPath))
	fmt.Printf("  2. QA imports it into %s (Export & Import -> Import).\n", extName)
	fmt.Printf("  3. QA opens %s, signs in via Entra ID SSO,\n", cfg.SitURL)
	fmt.Println("     and tests your local build seamlessly.")
	fmt.Println("============================================================")
	fmt.Println("Press Ctrl+C to close the tunnel and stop.")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		fmt.Printf("\n[airgapbridge] Received %s, shutting down cleanly...\n", sig)
	case <-tunInst.ExitCh:
		fmt.Println("\n[airgapbridge] cloudflared tunnel exited.")
	case srvErr := <-serverExitCh:
		if srvErr != nil {
			fmt.Printf("\n[airgapbridge] Local server exited unexpectedly: %v\n", srvErr)
		} else {
			fmt.Println("\n[airgapbridge] Local server exited.")
		}
	}

	cleanup()
	return nil
}

func ensureDevServerRunning(cwd string, localPort int, configCmd, pmOverride string, interactive bool) (*exec.Cmd, <-chan error, error) {
	if pm.IsPortListening(localPort) {
		fmt.Printf("[airgapbridge] Dev server already listening on port %d.\n", localPort)
		return nil, nil, nil
	}

	resolvedCmd, err := pm.ResolveDevCommand(cwd, pmOverride, configCmd)
	if err != nil {
		return nil, nil, err
	}

	fmt.Printf("[airgapbridge] Starting local dev server (%s)...\n", resolvedCmd)
	cmd, exitCh, err := pm.SpawnDevServerWithOptions(resolvedCmd, cwd, interactive)
	if err != nil {
		return nil, nil, err
	}

	timeout := 45 * time.Second
	if interactive {
		timeout = 120 * time.Second
	}

	if err := pm.WaitForPort(localPort, timeout, exitCh); err != nil {
		pm.KillProcessTree(cmd)
		return nil, nil, err
	}

	fmt.Printf("[airgapbridge] Dev server ready on http://localhost:%d\n", localPort)
	return cmd, exitCh, nil
}

func ensureQAServerRunning(cwd string, qaPort int, qaCmd string, skipBuild, interactive bool) (*exec.Cmd, <-chan error, error) {
	if skipBuild && pm.IsPortListening(qaPort) {
		fmt.Printf("[airgapbridge] QA build server already listening on port %d.\n", qaPort)
		return nil, nil, nil
	}

	fmt.Printf("[airgapbridge] Starting QA build server (%s)...\n", qaCmd)
	extraEnv := []string{fmt.Sprintf("PORT=%d", qaPort)}
	cmd, exitCh, err := pm.SpawnServerWithOptions(qaCmd, cwd, "[qa]", interactive, extraEnv...)
	if err != nil {
		return nil, nil, err
	}

	timeout := 45 * time.Second
	if interactive {
		timeout = 120 * time.Second
	}

	if err := pm.WaitForPort(qaPort, timeout, exitCh); err != nil {
		pm.KillProcessTree(cmd)
		return nil, nil, err
	}

	fmt.Printf("[airgapbridge] QA build server ready on http://localhost:%d\n", qaPort)
	return cmd, exitCh, nil
}
