package auth

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xyu/aco/internal/config"
)

func lookup(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

// cfgOrigin simulates config fields set by a file layer.
func cfgOrigin(set ...string) func(string) (config.LayerInfo, bool) {
	return func(f string) (config.LayerInfo, bool) {
		for _, s := range set {
			if s == f {
				return config.LayerInfo{Name: "repo", Source: ".aco.yaml"}, true
			}
		}
		return config.LayerInfo{}, false
	}
}

func TestResolvePrecedence(t *testing.T) {
	type want struct {
		claude, codex       Mode
		claudeSrc, codexSrc SourceKind
		claudeName          string
	}
	cases := []struct {
		name  string
		flags Flags
		env   map[string]string
		file  map[string]string
		cfg   config.AuthConfig
		set   []string // config fields set by a layer
		want  want
	}{
		{name: "default subscription",
			cfg:  config.AuthConfig{Mode: "subscription"},
			want: want{Subscription, Subscription, SourceDefault, SourceDefault, ""}},
		{name: "config global",
			cfg: config.AuthConfig{Mode: "api"}, set: []string{"auth.mode"},
			want: want{API, API, SourceConfig, SourceConfig, "auth.mode"}},
		{name: "config per-agent beats config global",
			cfg: config.AuthConfig{Mode: "api", Claude: "subscription"}, set: []string{"auth.mode", "auth.claude"},
			want: want{Subscription, API, SourceConfig, SourceConfig, "auth.claude"}},
		{name: "ACO_AUTH_MODE beats config",
			env: map[string]string{EnvAuthMode: "api"},
			cfg: config.AuthConfig{Mode: "subscription", Claude: "subscription"}, set: []string{"auth.mode", "auth.claude"},
			want: want{API, API, SourceEnv, SourceEnv, EnvAuthMode}},
		{name: "per-agent env beats ACO_AUTH_MODE",
			env:  map[string]string{EnvAuthMode: "api", EnvClaudeAuthMode: "subscription"},
			cfg:  config.AuthConfig{Mode: "subscription"},
			want: want{Subscription, API, SourceEnv, SourceEnv, EnvClaudeAuthMode}},
		{name: "global flag beats per-agent env",
			flags: Flags{AuthMode: "api"},
			env:   map[string]string{EnvClaudeAuthMode: "subscription"},
			cfg:   config.AuthConfig{Mode: "subscription"},
			want:  want{API, API, SourceFlag, SourceFlag, FlagAuthMode}},
		{name: "per-agent flag beats global flag",
			flags: Flags{AuthMode: "api", ClaudeAuthMode: "subscription"},
			cfg:   config.AuthConfig{Mode: "subscription"},
			want:  want{Subscription, API, SourceFlag, SourceFlag, FlagClaudeAuthMode}},
		{name: "mixed: claude subscription, codex api via env",
			env:  map[string]string{EnvCodexAuthMode: "api"},
			cfg:  config.AuthConfig{Mode: "subscription"},
			want: want{Subscription, API, SourceDefault, SourceEnv, ""}},
		{name: "mixed via env file",
			file: map[string]string{EnvAuthMode: "subscription", EnvCodexAuthMode: "api"},
			cfg:  config.AuthConfig{Mode: "subscription"},
			want: want{Subscription, API, SourceEnvFile, SourceEnvFile, EnvAuthMode}},
		{name: "process env beats env file for the same variable",
			env:  map[string]string{EnvCodexAuthMode: "subscription"},
			file: map[string]string{EnvCodexAuthMode: "api"},
			cfg:  config.AuthConfig{Mode: "subscription"},
			want: want{Subscription, Subscription, SourceDefault, SourceEnv, ""}},
		{name: "empty env values are unset",
			env:  map[string]string{EnvAuthMode: "", EnvClaudeAuthMode: "  "},
			file: map[string]string{EnvCodexAuthMode: ""},
			cfg:  config.AuthConfig{Mode: "subscription"},
			want: want{Subscription, Subscription, SourceDefault, SourceDefault, ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var ef *EnvFile
			if tc.file != nil {
				ef = &EnvFile{Path: "/home/u/.aco/.env", Found: true, Values: tc.file}
			}
			r, err := Resolve(tc.flags, NewEnv(lookup(tc.env), ef), tc.cfg, cfgOrigin(tc.set...))
			if err != nil {
				t.Fatal(err)
			}
			if r.Claude.Mode != tc.want.claude || r.Codex.Mode != tc.want.codex {
				t.Errorf("modes = %s/%s, want %s/%s", r.Claude.Mode, r.Codex.Mode, tc.want.claude, tc.want.codex)
			}
			if r.Claude.Source.Kind != tc.want.claudeSrc || r.Codex.Source.Kind != tc.want.codexSrc {
				t.Errorf("sources = %v/%v, want %s/%s", r.Claude.Source, r.Codex.Source, tc.want.claudeSrc, tc.want.codexSrc)
			}
			if r.Claude.Source.Name != tc.want.claudeName {
				t.Errorf("claude source name = %q, want %q", r.Claude.Source.Name, tc.want.claudeName)
			}
		})
	}
}

func TestResolveInvalidValuesNameVariable(t *testing.T) {
	cases := []struct {
		flags Flags
		env   map[string]string
		file  map[string]string
		cfg   config.AuthConfig
		want  Source
	}{
		{env: map[string]string{EnvAuthMode: "API-KEY"}, want: Source{Kind: SourceEnv, Name: EnvAuthMode}},
		{env: map[string]string{EnvClaudeAuthMode: "sub"}, want: Source{Kind: SourceEnv, Name: EnvClaudeAuthMode}},
		{env: map[string]string{EnvCodexAuthMode: "chatgpt"}, want: Source{Kind: SourceEnv, Name: EnvCodexAuthMode}},
		{env: map[string]string{EnvAuthFallback: "yes"}, want: Source{Kind: SourceEnv, Name: EnvAuthFallback}},
		{file: map[string]string{EnvCodexAuthMode: "x"}, want: Source{Kind: SourceEnvFile, Name: EnvCodexAuthMode, Path: "f.env"}},
		{flags: Flags{CodexAuthMode: "x"}, want: Source{Kind: SourceFlag, Name: FlagCodexAuthMode}},
		// Invalid even though a higher-precedence flag would win.
		{flags: Flags{AuthMode: "api"}, env: map[string]string{EnvAuthMode: "bogus"}, want: Source{Kind: SourceEnv, Name: EnvAuthMode}},
		{cfg: config.AuthConfig{Mode: "free"}, want: Source{Kind: SourceConfig, Name: "auth.mode"}},
	}
	for _, tc := range cases {
		var ef *EnvFile
		if tc.file != nil {
			ef = &EnvFile{Path: "f.env", Found: true, Values: tc.file}
		}
		if tc.cfg.Mode == "" {
			tc.cfg.Mode = "subscription"
		}
		_, err := Resolve(tc.flags, NewEnv(lookup(tc.env), ef), tc.cfg, nil)
		var es Errors
		if !errors.As(err, &es) || len(es) != 1 {
			t.Fatalf("%+v: want one error, got %v", tc, err)
		}
		if es[0].Source != tc.want {
			t.Errorf("error source = %+v, want %+v", es[0].Source, tc.want)
		}
	}
}

func TestFallback(t *testing.T) {
	cfg := config.AuthConfig{Mode: "subscription", Fallback: "none"}
	r, _ := Resolve(Flags{}, NewEnv(lookup(nil), nil), cfg, cfgOrigin())
	if r.Fallback != FallbackNone || r.FallbackSource.Kind != SourceDefault {
		t.Errorf("default fallback: %v %v", r.Fallback, r.FallbackSource)
	}
	r, _ = Resolve(Flags{}, NewEnv(lookup(map[string]string{EnvAuthFallback: "api"}), nil), cfg, cfgOrigin())
	if r.Fallback != FallbackAPI || r.FallbackSource.Name != EnvAuthFallback {
		t.Errorf("env fallback: %v %v", r.Fallback, r.FallbackSource)
	}
	cfg.Fallback = "api"
	r, _ = Resolve(Flags{}, NewEnv(lookup(nil), nil), cfg, cfgOrigin("auth.fallback"))
	if r.Fallback != FallbackAPI || r.FallbackSource.Kind != SourceConfig {
		t.Errorf("config fallback: %v %v", r.Fallback, r.FallbackSource)
	}
}

func TestCheckKeys(t *testing.T) {
	cfg := config.AuthConfig{Mode: "subscription"}
	none := NewEnv(lookup(nil), nil)

	r, _ := Resolve(Flags{}, none, cfg, nil)
	if ks := CheckKeys(r, none, nil); len(ks) != 0 {
		t.Errorf("subscription needs no keys: %+v", ks)
	}

	r, _ = Resolve(Flags{ClaudeAuthMode: "api"}, none, cfg, nil)
	ks := CheckKeys(r, none, nil)
	if len(ks) != 1 || ks[0].Var != EnvAnthropicKey || ks[0].Present || ks[0].Reason != ReasonAPIMode {
		t.Errorf("claude api without key: %+v", ks)
	}

	file := &EnvFile{Path: "p", Found: true, Values: map[string]string{EnvOpenAIKey: "sk-test-123456789"}}
	env := NewEnv(lookup(map[string]string{EnvAnthropicKey: "sk-ant-test-123456"}), file)
	r, _ = Resolve(Flags{AuthMode: "api"}, env, cfg, nil)
	ks = CheckKeys(r, env, nil)
	if len(ks) != 2 || !ks[0].Present || ks[0].Source.Kind != SourceEnv || !ks[1].Present || ks[1].Source.Kind != SourceEnvFile {
		t.Errorf("both keys present: %+v", ks)
	}

	fb := NewEnv(lookup(map[string]string{EnvAuthFallback: "api"}), nil)
	r, _ = Resolve(Flags{}, fb, cfg, nil)
	ks = CheckKeys(r, fb, nil)
	if len(ks) != 2 || ks[0].Reason != ReasonFallback || ks[0].Present {
		t.Errorf("fallback keys: %+v", ks)
	}

	ks = CheckKeys(r, fb, fakeKeychain{EnvAnthropicKey: "sk-ant-from-keychain"})
	if !ks[0].Present || ks[0].Source.Kind != SourceKeychain || ks[1].Present {
		t.Errorf("keychain: %+v", ks)
	}
}

type fakeKeychain map[string]string

func (f fakeKeychain) Lookup(n string) (string, bool) { v, ok := f[n]; return v, ok }

func TestParseEnv(t *testing.T) {
	in := "# comment\n\nACO_AUTH_MODE=api\nexport ACO_CODEX_AUTH_MODE = subscription\r\n" +
		"EMPTY=\nQUOTED=\"a b\\n#c\"\nSINGLE='x \\n y'\nINLINE=value # trailing\n"
	got, err := ParseEnv([]byte("\xef\xbb\xbf"+in), "f")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"ACO_AUTH_MODE": "api", "ACO_CODEX_AUTH_MODE": "subscription", "EMPTY": "",
		"QUOTED": "a b\n#c", "SINGLE": `x \n y`, "INLINE": "value",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	for _, bad := range []string{"NOEQUALS\n", "1BAD=x\n", "Q=\"unterminated\n", "S='open\n"} {
		_, err := ParseEnv([]byte("OK=1\n"+bad), "f.env")
		var fe *EnvFileError
		if !errors.As(err, &fe) || fe.Line != 2 {
			t.Errorf("%q: got %v", bad, err)
		}
	}
	// Syntax errors never echo the line content (it may hold a secret).
	_, err = ParseEnv([]byte("sk-ant-secretvalue123\n"), "f.env")
	if err == nil || strings.Contains(err.Error(), "secretvalue") {
		t.Errorf("error leaks content: %v", err)
	}
}

func TestLoadEnvFile(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "none.env")
	ef, err := LoadEnvFile(missing, false)
	if err != nil || ef.Found {
		t.Errorf("optional missing file: %+v %v", ef, err)
	}
	if _, err := LoadEnvFile(missing, true); err == nil {
		t.Error("explicit missing file must fail")
	}
	p := filepath.Join(dir, ".env")
	if err := os.WriteFile(p, []byte("ACO_AUTH_MODE=api\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ef, err = LoadEnvFile(p, true)
	if err != nil || !ef.Found || ef.Values[EnvAuthMode] != "api" || ef.LoosePerms {
		t.Errorf("load: %+v %v", ef, err)
	}
}

func TestExpandHome(t *testing.T) {
	if got := ExpandHome("~/.aco/.env", "/h"); got != filepath.Join("/h", ".aco/.env") {
		t.Errorf("got %q", got)
	}
	if got := ExpandHome("/abs/x", "/h"); got != "/abs/x" {
		t.Errorf("got %q", got)
	}
}
