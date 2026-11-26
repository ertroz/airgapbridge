package tunnel

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"airgapbridge/internal/config"
	"airgapbridge/internal/pm"
)

const QARulesFilename = "airgapbridge-qa-rules.json"

var (
	tryCloudflareRe  = regexp.MustCompile(`https://[a-zA-Z0-9-]+\.trycloudflare\.com`)
	leadingBypassRe  = regexp.MustCompile(`^\^?(?:\\?\/)+`)
	unescapedSlashRe = regexp.MustCompile(`([^\\])\/`)
	regexMetaRe      = regexp.MustCompile(`[.*+?^${}()|[\]\\/]`)
)

type Instance struct {
	URL    string
	Cmd    *exec.Cmd
	ExitCh <-chan error
}

func (t *Instance) Close() {
	if t == nil {
		return
	}
	pm.KillProcessTree(t.Cmd)
}

type RequestlySource struct {
	Key      string `json:"key"`
	Operator string `json:"operator"`
	Value    string `json:"value"`
}

type RequestlyPair struct {
	Destination string          `json:"destination"`
	Source      RequestlySource `json:"source"`
}

type RequestlyRule struct {
	CreationDate int64           `json:"creationDate"`
	Description  string          `json:"description"`
	GroupID      string          `json:"groupId"`
	IsSample     bool            `json:"isSample"`
	Name         string          `json:"name"`
	ObjectType   string          `json:"objectType"`
	Pairs        []RequestlyPair `json:"pairs"`
	RuleType     string          `json:"ruleType"`
	Status       string          `json:"status"`
}

func EscapeRegex(s string) string {
	return regexMetaRe.ReplaceAllString(s, `\$0`)
}

func normalizeBypassPattern(pattern string) string {
	stripped := leadingBypassRe.ReplaceAllString(strings.TrimSpace(pattern), "")
	if strings.HasPrefix(stripped, "/") {
		stripped = `\/` + stripped[1:]
	}
	return unescapedSlashRe.ReplaceAllString(stripped, `${1}\/`)
}

func BuildRequestlySourceRegex(sitURL string, bypassPaths []string) (string, error) {
	normSit, err := config.NormalizeSitURL(sitURL)
	if err != nil {
		return "", err
	}
	escapedSit := EscapeRegex(normSit)

	cleaned := make([]string, 0, len(bypassPaths))
	for _, p := range bypassPaths {
		if norm := normalizeBypassPattern(p); norm != "" {
			cleaned = append(cleaned, norm)
		}
	}

	if len(cleaned) == 0 {
		return fmt.Sprintf(`^%s\/(.*)`, escapedSit), nil
	}
	return fmt.Sprintf(`^%s\/(?!%s)(.*)`, escapedSit, strings.Join(cleaned, "|")), nil
}

func GenerateRequestlyRules(sitURL, tunnelURL string, bypassPaths []string) ([]RequestlyRule, error) {
	normSit, err := config.NormalizeSitURL(sitURL)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(normSit)
	if err != nil {
		return nil, err
	}
	cleanTunnel := strings.TrimRight(tunnelURL, "/")
	sourceRegex, err := BuildRequestlySourceRegex(normSit, bypassPaths)
	if err != nil {
		return nil, err
	}

	return []RequestlyRule{
		{
			CreationDate: time.Now().UnixMilli(),
			Description:  fmt.Sprintf("AirgapBridge QA redirect rule: %s -> %s", normSit, cleanTunnel),
			GroupID:      "",
			IsSample:     false,
			Name:         fmt.Sprintf("AirgapBridge QA - %s", u.Host),
			ObjectType:   "rule",
			Pairs: []RequestlyPair{
				{
					Destination: fmt.Sprintf("%s/$1", cleanTunnel),
					Source: RequestlySource{
						Key:      "Url",
						Operator: "Matches",
						Value:    sourceRegex,
					},
				},
			},
			RuleType: "Redirect",
			Status:   "Active",
		},
	}, nil
}

func WriteRequestlyRulesFile(cwd, sitURL, tunnelURL string, bypassPaths []string, outputFile string) (string, error) {
	if outputFile == "" {
		outputFile = QARulesFilename
	}
	rules, err := GenerateRequestlyRules(sitURL, tunnelURL, bypassPaths)
	if err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(rules, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')

	outPath := outputFile
	if !filepath.IsAbs(outPath) {
		outPath = filepath.Join(cwd, outputFile)
	}
	if err := os.WriteFile(outPath, data, 0o644); err != nil {
		return "", err
	}
	return outPath, nil
}

func EnsureCloudflaredInstalled() error {
	if _, err := exec.LookPath("cloudflared"); err == nil {
		return nil
	}
	var hint string
	switch runtime.GOOS {
	case "darwin":
		hint = "  macOS:   brew install cloudflared"
	case "windows":
		hint = "  Windows: winget install --id Cloudflare.cloudflared"
	default:
		hint = "  Linux:   https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/downloads/"
	}
	return fmt.Errorf("\"cloudflared\" is not installed or not in PATH.\nInstall it to use \"airgapbridge qa\":\n%s", hint)
}

func StartCloudflaredTunnel(localPort int, timeout time.Duration) (*Instance, error) {
	if err := EnsureCloudflaredInstalled(); err != nil {
		return nil, err
	}

	cmd := exec.Command("cloudflared", "tunnel", "--url", fmt.Sprintf("http://localhost:%d", localPort))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start cloudflared: %w", err)
	}

	urlCh := make(chan string, 1)
	exitCh := make(chan error, 1)

	scanPipe := func(r io.Reader) {
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			line := scanner.Text()
			if m := tryCloudflareRe.FindString(line); m != "" {
				select {
				case urlCh <- m:
				default:
				}
			}
		}
	}

	go scanPipe(stdout)
	go scanPipe(stderr)
	go func() {
		exitCh <- cmd.Wait()
	}()

	select {
	case tunnelURL := <-urlCh:
		return &Instance{
			URL:    tunnelURL,
			Cmd:    cmd,
			ExitCh: exitCh,
		}, nil
	case err := <-exitCh:
		return nil, fmt.Errorf("cloudflared exited unexpectedly: %v", err)
	case <-time.After(timeout):
		pm.KillProcessTree(cmd)
		return nil, fmt.Errorf("timed out waiting for cloudflared tunnel URL after %s", timeout)
	}
}

func CopyToClipboard(text string) bool {
	type clipCmd struct {
		name string
		args []string
	}
	var candidates []clipCmd
	switch runtime.GOOS {
	case "darwin":
		candidates = []clipCmd{{name: "pbcopy"}}
	case "windows":
		candidates = []clipCmd{{name: "clip"}}
	default:
		candidates = []clipCmd{
			{name: "wl-copy"},
			{name: "xclip", args: []string{"-selection", "clipboard"}},
			{name: "xsel", args: []string{"--clipboard", "--input"}},
		}
	}

	for _, c := range candidates {
		if _, err := exec.LookPath(c.name); err != nil {
			continue
		}
		cmd := exec.Command(c.name, c.args...)
		cmd.Stdin = strings.NewReader(text)
		if err := cmd.Run(); err == nil {
			return true
		}
	}
	return false
}
