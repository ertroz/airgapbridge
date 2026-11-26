package main

import (
	"encoding/json"
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

const version = "1.1.0"

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
  qa         Expose local dev server via cloudflared and generate Requestly QA rules
  version    Print CLI version
  help       Show this help message

Run "airgapbridge <command> --help" for command-specific flags.
`, version)
}

func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	sitURL := fs.String("sit-url", "", "Target SIT frontend URL (e.g. https://sit-app.company.com)")
	port := fs.Int("port", 0, "Local dev server port")
	pmFlag := fs.String("pm", "", "Package manager override (bun | npm | yarn | pnpm)")
	devCmd := fs.String("dev-command", "", "Custom local dev command")
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
		SitURL:     *sitURL,
		Port:       *port,
		PM:         *pmFlag,
		DevCommand: *devCmd,
		Bypass:     *bypass,
		Yes:        *yes,
	})
	if err != nil {
		return err
	}

	pretty, _ := json.MarshalIndent(cfg, "", "  ")
	fmt.Printf("\n[airgapbridge] Created %s:\n%s\n", config.ConfigFilename, string(pretty))
	config.CheckFrameworkCompatibility(cwd, cfg.SitURL)
	fmt.Println("\nNext steps:")
	fmt.Println("  1. Run \"airgapbridge dev\" for local SSO development")
	fmt.Println("  2. Run \"airgapbridge qa\"  to share a pre-PR build with QA")
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

	devCmd, err = ensureDevServerRunning(cwd, localPort, cfg.DevCommand, *pmFlag, interactive)
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
	portFlag := fs.Int("port", 0, "Override local dev server port")
	outputFile := fs.String("output", tunnel.QARulesFilename, "Output Requestly rules filename")
	fs.StringVar(outputFile, "o", tunnel.QARulesFilename, "Output Requestly rules filename")
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

	devCmd, err = ensureDevServerRunning(cwd, localPort, cfg.DevCommand, *pmFlag, interactive)
	if err != nil {
		return err
	}

	fmt.Printf("[airgapbridge] Starting cloudflared tunnel (%s) to http://localhost:%d...\n", *protocolFlag, localPort)
	tunInst, err = tunnel.StartCloudflaredTunnel(localPort, 30*time.Second, *protocolFlag)
	if err != nil {
		return err
	}

	var rulesPath string
	switch *formatFlag {
	case "header-editor":
		rulesPath, err = tunnel.WriteHeaderEditorRulesFile(cwd, cfg.SitURL, tunInst.URL, cfg.BypassPaths, *outputFile)
	case "requestly":
		rulesPath, err = tunnel.WriteRequestlyRulesFile(cwd, cfg.SitURL, tunInst.URL, cfg.BypassPaths, *outputFile)
	default:
		return fmt.Errorf("invalid --format %q (supported: header-editor, requestly)", *formatFlag)
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
	if *formatFlag == "requestly" {
		extName = "Requestly"
	}

	fmt.Println("\n============================================================")
	fmt.Println("  AirgapBridge QA Tunnel Ready")
	fmt.Println("============================================================")
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
	}

	cleanup()
	return nil
}

func ensureDevServerRunning(cwd string, localPort int, configCmd, pmOverride string, interactive bool) (*exec.Cmd, error) {
	if pm.IsPortListening(localPort) {
		fmt.Printf("[airgapbridge] Dev server already listening on port %d.\n", localPort)
		return nil, nil
	}

	resolvedCmd, err := pm.ResolveDevCommand(cwd, pmOverride, configCmd)
	if err != nil {
		return nil, err
	}

	fmt.Printf("[airgapbridge] Starting local dev server (%s)...\n", resolvedCmd)
	cmd, exitCh, err := pm.SpawnDevServerWithOptions(resolvedCmd, cwd, interactive)
	if err != nil {
		return nil, err
	}

	timeout := 45 * time.Second
	if interactive {
		timeout = 120 * time.Second
	}

	if err := pm.WaitForPort(localPort, timeout, exitCh); err != nil {
		pm.KillProcessTree(cmd)
		return nil, err
	}

	fmt.Printf("[airgapbridge] Dev server ready on http://localhost:%d\n", localPort)
	return cmd, nil
}
