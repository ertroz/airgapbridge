package tunnel

import (
	"testing"
)

func TestBuildRequestlySourceRegex(t *testing.T) {
	reStr, err := BuildRequestlySourceRegex(
		"https://sit-app.company.com",
		[]string{"^/api/auth/.*", "^/login/callback.*"},
	)
	if err != nil {
		t.Fatalf("BuildRequestlySourceRegex failed: %v", err)
	}

	want := `^https:\/\/sit-app\.company\.com\/(?!api\/auth\/.*|login\/callback.*)(.*)`
	if reStr != want {
		t.Fatalf("regex mismatch:\ngot:  %s\nwant: %s", reStr, want)
	}

	rules, err := GenerateRequestlyRules(
		"https://sit-app.company.com",
		"https://demo-abc.trycloudflare.com",
		[]string{"^/api/auth/.*"},
	)
	if err != nil {
		t.Fatalf("GenerateRequestlyRules failed: %v", err)
	}
	if len(rules) != 1 || rules[0].RuleType != "Redirect" || rules[0].Pairs[0].Destination != "https://demo-abc.trycloudflare.com/$1" {
		t.Fatalf("unexpected rule output: %+v", rules)
	}
}

func TestGenerateHeaderEditorConfig(t *testing.T) {
	cfg, err := GenerateHeaderEditorConfig(
		"https://sit-app.company.com",
		"https://demo-abc.trycloudflare.com",
		[]string{"^/api/auth/.*", "^/login/callback.*"},
	)
	if err != nil {
		t.Fatalf("GenerateHeaderEditorConfig failed: %v", err)
	}
	if len(cfg.Request) != 1 {
		t.Fatalf("expected 1 request rule, got %d", len(cfg.Request))
	}
	r := cfg.Request[0]
	if r.RuleType != "redirect" || r.MatchType != "regexp" || r.Action != "redirect" {
		t.Fatalf("unexpected rule types: %+v", r)
	}
	if r.Pattern != `^https:\/\/sit-app\.company\.com\/(.*)` {
		t.Fatalf("unexpected pattern: %s", r.Pattern)
	}
	if r.Exclude != `^https:\/\/sit-app\.company\.com\/(api\/auth\/.*|login\/callback.*)` {
		t.Fatalf("unexpected exclude: %s", r.Exclude)
	}
	if r.To != "https://demo-abc.trycloudflare.com/$1" {
		t.Fatalf("unexpected destination: %s", r.To)
	}
}
