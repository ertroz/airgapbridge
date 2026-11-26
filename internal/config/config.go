package config

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"airgapbridge/internal/pm"
)

const (
	ConfigFilename   = "airgapbridge.config.json"
	DefaultProxyPort = 8899
)

var DefaultBypassPaths = []string{"^/api/auth/.*", "^/login/callback.*"}

type Framework string

const (
	FrameworkNextJS  Framework = "nextjs"
	FrameworkVite    Framework = "vite"
	FrameworkUnknown Framework = "unknown"
)

type Config struct {
	SitURL         string   `json:"sitUrl"`
	LocalPort      int      `json:"localPort"`
	DevCommand     string   `json:"devCommand"`
	BypassPaths    []string `json:"bypassPaths"`
	TunnelProvider string   `json:"tunnelProvider"`
	ProxyPort      int      `json:"proxyPort,omitempty"`
}

type InitOptions struct {
	SitURL     string
	Port       int
	PM         string
	DevCommand string
	Bypass     string
	Yes        bool
}

func NormalizeSitURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("SIT URL cannot be empty")
	}
	if !strings.HasPrefix(strings.ToLower(trimmed), "http://") &&
		!strings.HasPrefix(strings.ToLower(trimmed), "https://") {
		trimmed = "https://" + trimmed
	}
	u, err := url.Parse(trimmed)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid SIT URL %q (expected e.g. https://sit-app.company.com)", raw)
	}
	return fmt.Sprintf("%s://%s", u.Scheme, u.Host), nil
}

func DetectFramework(cwd string) (Framework, int) {
	pkgPath := filepath.Join(cwd, "package.json")
	data, err := os.ReadFile(pkgPath)
	if err != nil {
		return FrameworkUnknown, 3000
	}
	var pkg struct {
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return FrameworkUnknown, 3000
	}
	if _, ok := pkg.Dependencies["next"]; ok {
		return FrameworkNextJS, 3000
	}
	if _, ok := pkg.DevDependencies["next"]; ok {
		return FrameworkNextJS, 3000
	}
	if _, ok := pkg.Dependencies["vite"]; ok {
		return FrameworkVite, 5173
	}
	if _, ok := pkg.DevDependencies["vite"]; ok {
		return FrameworkVite, 5173
	}
	return FrameworkUnknown, 3000
}

var (
	viteCorsRe         = regexp.MustCompile(`cors\s*:\s*true`)
	viteAllowedHostsRe = regexp.MustCompile(`allowedHosts\s*:`)
)

func CheckFrameworkCompatibility(cwd, sitURL string) {
	fw, _ := DetectFramework(cwd)
	sitHost := "sit-app.company.com"
	if norm, err := NormalizeSitURL(sitURL); err == nil {
		if u, err := url.Parse(norm); err == nil && u.Hostname() != "" {
			sitHost = u.Hostname()
		}
	}

	switch fw {
	case FrameworkNextJS:
		for _, name := range []string{"next.config.ts", "next.config.mjs", "next.config.js", "next.config.cjs"} {
			p := filepath.Join(cwd, name)
			data, err := os.ReadFile(p)
			if err == nil {
				if !strings.Contains(string(data), "allowedDevOrigins") {
					fmt.Fprintf(os.Stderr,
						"\n[airgapbridge] Warning: %s does not configure \"allowedDevOrigins\".\n"+
							"If Next.js blocks HMR or cross-origin requests from %s, add:\n"+
							"  // %s\n"+
							"  allowedDevOrigins: ['%s', '*.trycloudflare.com'],\n\n",
						name, sitHost, name, sitHost,
					)
				}
				return
			}
		}
	case FrameworkVite:
		for _, name := range []string{"vite.config.ts", "vite.config.mts", "vite.config.js", "vite.config.mjs"} {
			p := filepath.Join(cwd, name)
			data, err := os.ReadFile(p)
			if err == nil {
				content := string(data)
				if !viteCorsRe.MatchString(content) || !viteAllowedHostsRe.MatchString(content) {
					fmt.Fprintf(os.Stderr,
						"\n[airgapbridge] Warning: %s may block proxied or tunneled hosts.\n"+
							"If Vite rejects requests from %s or *.trycloudflare.com, add:\n"+
							"  server: {\n"+
							"    cors: true,\n"+
							"    allowedHosts: true,\n"+
							"  },\n\n",
						name, sitHost,
					)
				}
				return
			}
		}
	}
}

func Load(cwd string) (*Config, error) {
	p := filepath.Join(cwd, ConfigFilename)
	data, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("missing %s in %s; run \"airgapbridge init\" first", ConfigFilename, cwd)
		}
		return nil, err
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse %s: %w", ConfigFilename, err)
	}
	if strings.TrimSpace(cfg.SitURL) == "" {
		return nil, fmt.Errorf("%s is missing required field \"sitUrl\"", ConfigFilename)
	}
	normURL, err := NormalizeSitURL(cfg.SitURL)
	if err != nil {
		return nil, err
	}
	cfg.SitURL = normURL

	if cfg.LocalPort <= 0 {
		_, defPort := DetectFramework(cwd)
		cfg.LocalPort = defPort
	}
	if strings.TrimSpace(cfg.DevCommand) == "" {
		cfg.DevCommand = pm.DefaultDevCommand(pm.DetectPackageManager(cwd))
	}
	if cfg.BypassPaths == nil {
		cfg.BypassPaths = append([]string{}, DefaultBypassPaths...)
	}
	if cfg.TunnelProvider == "" {
		cfg.TunnelProvider = "cloudflared"
	}
	if cfg.ProxyPort <= 0 {
		cfg.ProxyPort = DefaultProxyPort
	}
	return &cfg, nil
}

func Save(cwd string, cfg *Config) (string, error) {
	p := filepath.Join(cwd, ConfigFilename)
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')
	if err := os.WriteFile(p, data, 0o644); err != nil {
		return "", err
	}
	return p, nil
}

func RunInitWizard(cwd string, opts InitOptions) (*Config, error) {
	fw, defPort := DetectFramework(cwd)
	detectedPM := pm.DetectPackageManager(cwd)
	if opts.PM != "" {
		if !pm.IsValidPackageManager(opts.PM) {
			return nil, fmt.Errorf("invalid package manager %q (supported: bun, npm, yarn, pnpm)", opts.PM)
		}
		detectedPM = pm.PackageManager(opts.PM)
	}

	defCmd := opts.DevCommand
	if defCmd == "" {
		defCmd = pm.DefaultDevCommand(detectedPM)
	}

	sitURL := opts.SitURL
	localPort := opts.Port
	if localPort <= 0 {
		localPort = defPort
	}
	devCmd := defCmd
	bypassPaths := append([]string{}, DefaultBypassPaths...)
	if opts.Bypass != "" {
		bypassPaths = splitCSV(opts.Bypass)
	}

	nonInteractive := opts.Yes || opts.SitURL != ""
	if !nonInteractive && isStdinTTY() {
		reader := bufio.NewReader(os.Stdin)
		fmt.Printf("\nAirgapBridge Setup (detected framework: %s, package manager: %s)\n\n", fw, detectedPM)

		fmt.Print("Target SIT URL (e.g. https://sit-app.company.com): ")
		ans, _ := reader.ReadString('\n')
		ans = strings.TrimSpace(ans)
		if ans == "" {
			return nil, errors.New("SIT URL is required")
		}
		sitURL = ans

		fmt.Printf("Local dev server port (%d): ", localPort)
		portAns, _ := reader.ReadString('\n')
		portAns = strings.TrimSpace(portAns)
		if portAns != "" {
			p, err := strconv.Atoi(portAns)
			if err != nil || p <= 0 || p > 65535 {
				return nil, fmt.Errorf("invalid port %q", portAns)
			}
			localPort = p
		}

		fmt.Printf("Local dev command (%s): ", devCmd)
		cmdAns, _ := reader.ReadString('\n')
		cmdAns = strings.TrimSpace(cmdAns)
		if cmdAns != "" {
			devCmd = cmdAns
		}

		fmt.Printf("Bypass path regexes, comma-separated (%s): ", strings.Join(DefaultBypassPaths, ", "))
		bpAns, _ := reader.ReadString('\n')
		bpAns = strings.TrimSpace(bpAns)
		if bpAns != "" {
			bypassPaths = splitCSV(bpAns)
		}
	} else if sitURL == "" {
		return nil, errors.New("missing --sit-url in non-interactive mode (example: airgapbridge init --sit-url https://sit-app.company.com)")
	}

	normURL, err := NormalizeSitURL(sitURL)
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		SitURL:         normURL,
		LocalPort:      localPort,
		DevCommand:     devCmd,
		BypassPaths:    bypassPaths,
		TunnelProvider: "cloudflared",
	}

	if _, err := Save(cwd, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func isStdinTTY() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}
