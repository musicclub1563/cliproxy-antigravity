package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// signedInHome returns a temporary agy home that looks like a completed login.
func signedInHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	stateDir := agyStateDir(home)
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("create agy state dir: %v", err)
	}
	token := []byte(`{"id_token_email":"account@example.com"}`)
	if err := os.WriteFile(filepath.Join(stateDir, "antigravity-oauth-token"), token, 0o600); err != nil {
		t.Fatalf("write oauth token: %v", err)
	}
	return home
}

func writeAuthFile(t *testing.T, dir, name string, meta map[string]any) {
	t.Helper()
	raw, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal auth file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), raw, 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
}

func fakeBinaryName() string {
	if runtime.GOOS == "windows" {
		return "agy.exe"
	}
	return "agy"
}

// buildFakeAgy compiles a stand-in for the official CLI. Tests never touch agy itself.
func buildFakeAgy(t *testing.T, source string) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain is required to build the fake agy CLI")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "fake_agy_main.go")
	if err := os.WriteFile(src, []byte(source), 0o644); err != nil {
		t.Fatalf("write fake agy source: %v", err)
	}
	bin := filepath.Join(dir, fakeBinaryName())
	cmd := exec.Command("go", "build", "-o", bin, src)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fake agy: %v\n%s", err, output)
	}
	if runtime.GOOS != "windows" {
		_ = os.Chmod(bin, 0o755)
	}
	return bin
}

const fakeAgyHomeGateSource = `package main

import (
	"fmt"
	"os"
)

const expectedHome = %q

func main() {
	if len(os.Args) < 2 || os.Args[1] != "models" {
		os.Exit(2)
	}
	if os.Getenv("HOME") != expectedHome {
		fmt.Fprintln(os.Stderr, "Error: Please sign in to view available models. Launch the CLI without arguments to sign in.")
		os.Exit(1)
	}
	for _, model := range []string{
		"gemini-3.8-flash-high",
		"gemini-3.8-flash-medium",
		"gemini-3.8-flash-low",
		"claude-sonnet-4-6",
		"claude-opus-4-6-thinking",
		"gpt-oss-120b-medium",
	} {
		fmt.Println(model)
	}
}
`

const fakeAgyTwoAccountsSource = `package main

import (
	"fmt"
	"os"
)

const homeA = %q
const homeB = %q

func printModels(models ...string) {
	for _, model := range models {
		fmt.Println(model)
	}
}

func main() {
	if len(os.Args) < 2 || os.Args[1] != "models" {
		os.Exit(2)
	}
	switch os.Getenv("HOME") {
	case homeA:
		printModels("model-a-one", "model-a-two")
	case homeB:
		printModels("model-b-one")
	default:
		fmt.Fprintln(os.Stderr, "Error: Please sign in to view available models.")
		os.Exit(1)
	}
	if path := os.Getenv("FAKE_AGY_COUNTER"); path != "" {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintln(file, os.Getenv("HOME"))
			file.Close()
		}
	}
}
`

const fakeAgyAnyHomeSource = `package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "models" {
		os.Exit(2)
	}
	fmt.Println("legacy-model-one")
}
`

const fakeAgyFailingSource = `package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Println("evil-model-one")
	fmt.Println("evil-model-two")
	fmt.Fprintln(os.Stderr, "Error: quota exceeded for leaky-user@example.com token=0123456789abcdef0123456789abcdef0123456789abcdef")
	os.Exit(1)
}
`

func modelIDs(models []modelInfo) []string {
	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	return ids
}

func hasModelID(models []modelInfo, id string) bool {
	for _, model := range models {
		if strings.EqualFold(model.ID, id) {
			return true
		}
	}
	return false
}

func envLast(env []string, key string) string {
	prefix := key + "="
	value := ""
	for _, item := range env {
		if strings.HasPrefix(item, prefix) {
			value = strings.TrimPrefix(item, prefix)
		}
	}
	return value
}

func TestModelsCommandUsesAccountHome(t *testing.T) {
	cfg := defaultConfig()
	home := signedInHome(t)
	accountCfg, err := withAccount(cfg, map[string]any{"home": home})
	if err != nil {
		t.Fatalf("withAccount: %v", err)
	}
	cmd, err := newModelsCommand(context.Background(), accountCfg)
	if err != nil {
		t.Fatalf("newModelsCommand: %v", err)
	}
	if got := envLast(cmd.Env, "HOME"); got != home {
		t.Fatalf("HOME = %q, want %q", got, home)
	}
	if got := envLast(cmd.Env, "USERPROFILE"); got != home {
		t.Fatalf("USERPROFILE = %q, want %q", got, home)
	}
	if len(cmd.Args) < 2 || cmd.Args[len(cmd.Args)-1] != "models" {
		t.Fatalf("args = %#v, want a trailing \"models\"", cmd.Args)
	}
	if !reflect.DeepEqual(cmd.Args[1:], []string{"models"}) {
		t.Fatalf("args = %#v, want [models]", cmd.Args)
	}

	// The host process environment must not be rewritten.
	if os.Getenv("HOME") == home {
		t.Fatalf("host HOME was mutated to the account home")
	}

	// Without an account the child simply inherits the environment.
	plain, err := newModelsCommand(context.Background(), cfg)
	if err != nil {
		t.Fatalf("newModelsCommand: %v", err)
	}
	if plain.Env != nil {
		t.Fatalf("expected inherited environment, got %#v", plain.Env)
	}
}

func TestDiscoverModelsUsesAccountHomeFromAuthFile(t *testing.T) {
	invalidateModels()
	authDir := t.TempDir()
	accountHome := signedInHome(t)
	writeAuthFile(t, authDir, "antigravity-user@example.com.json", map[string]any{
		"type":     "agy",
		"email":    "user@example.com",
		"home":     accountHome,
		"disabled": false,
	})
	setAuthDir(authDir)
	t.Cleanup(func() {
		setAuthDir("")
		invalidateModels()
	})

	cfg := defaultConfig()
	cfg.BinaryPath = buildFakeAgy(t, fmt.Sprintf(fakeAgyHomeGateSource, accountHome))

	models := discoverModels(cfg)

	for _, want := range []string{
		"agy/gemini-3.8-flash-high",
		"agy/gemini-3.8-flash-medium",
		"agy/gemini-3.8-flash-low",
		"agy/claude-sonnet-4-6",
		"agy/claude-opus-4-6-thinking",
		"agy/gpt-oss-120b-medium",
	} {
		if !hasModelID(models, want) {
			t.Fatalf("model %q missing from %#v", want, modelIDs(models))
		}
	}

	// The sign-in failure text must never be treated as a model.
	for _, id := range modelIDs(models) {
		lower := strings.ToLower(id)
		if strings.Contains(lower, "sign in") || strings.Contains(lower, "error") {
			t.Fatalf("error output leaked into models: %q", id)
		}
	}

	if native := resolveNativeModel("agy/claude-sonnet-4-6", cfg); native != "claude-sonnet-4-6" {
		t.Fatalf("native model for agy/claude-sonnet-4-6 = %q", native)
	}
	if native := resolveNativeModel("agy/gpt-oss-120b-medium", cfg); native != "gpt-oss-120b-medium" {
		t.Fatalf("native model for agy/gpt-oss-120b-medium = %q", native)
	}
}

func TestDiscoverModelsSkipsOutputOfFailedCommand(t *testing.T) {
	invalidateModels()
	authDir := t.TempDir()
	accountHome := signedInHome(t)
	writeAuthFile(t, authDir, "antigravity-failing.json", map[string]any{
		"type":  "agy",
		"email": "leaky-user@example.com",
		"home":  accountHome,
	})
	setAuthDir(authDir)
	t.Cleanup(func() {
		setAuthDir("")
		invalidateModels()
	})

	cfg := defaultConfig()
	cfg.BinaryPath = buildFakeAgy(t, fakeAgyFailingSource)

	models := discoverModels(cfg)
	if hasModelID(models, "agy/evil-model-one") || hasModelID(models, "agy/evil-model-two") {
		t.Fatalf("stdout of a failed agy run was parsed as models: %#v", modelIDs(models))
	}
	if !hasModelID(models, "agy/default") {
		t.Fatalf("built-in fallback is missing: %#v", modelIDs(models))
	}

	diagnostic := modelCatalog.diagnostic
	if !strings.Contains(diagnostic, "failed model discovery") {
		t.Fatalf("expected a discovery diagnostic, got %q", diagnostic)
	}
	if strings.Contains(diagnostic, "leaky-user@example.com") {
		t.Fatalf("diagnostic leaked an email: %q", diagnostic)
	}
	if strings.Contains(diagnostic, "0123456789abcdef") {
		t.Fatalf("diagnostic leaked a token: %q", diagnostic)
	}
}

func TestDiscoverModelsKeepsLegacyProcessHomeFallback(t *testing.T) {
	invalidateModels()
	// No auth file and no accounts_dir, as on a single-account install.
	setAuthDir(t.TempDir())
	t.Cleanup(func() {
		setAuthDir("")
		invalidateModels()
	})

	cfg := defaultConfig()
	cfg.BinaryPath = buildFakeAgy(t, fakeAgyAnyHomeSource)

	models := discoverModels(cfg)
	if !hasModelID(models, "agy/legacy-model-one") {
		t.Fatalf("legacy discovery fallback did not run agy: %#v", modelIDs(models))
	}
}

func TestModelCatalogCacheIsPerAccount(t *testing.T) {
	invalidateModels()
	homeA := signedInHome(t)
	homeB := signedInHome(t)
	authDir := t.TempDir()
	setAuthDir(authDir)
	counter := filepath.Join(t.TempDir(), "calls.log")
	t.Setenv("FAKE_AGY_COUNTER", counter)
	t.Cleanup(func() {
		setAuthDir("")
		invalidateModels()
	})

	cfg := defaultConfig()
	cfg.BinaryPath = buildFakeAgy(t, fmt.Sprintf(fakeAgyTwoAccountsSource, homeA, homeB))

	writeAuthFile(t, authDir, "antigravity-a.json", map[string]any{"type": "agy", "home": homeA, "email": "a@example.com"})
	modelsA := discoverModels(cfg)
	if !hasModelID(modelsA, "agy/model-a-one") {
		t.Fatalf("account A models missing: %#v", modelIDs(modelsA))
	}
	if hasModelID(modelsA, "agy/model-b-one") {
		t.Fatalf("account B models leaked into account A: %#v", modelIDs(modelsA))
	}

	// A second, unchanged call must hit the cache instead of spawning agy again.
	_ = discoverModels(cfg)
	if calls := countLines(t, counter); calls != 1 {
		t.Fatalf("agy models was spawned %d times for one account, want 1", calls)
	}

	// A different account set must not reuse account A's cached list.
	writeAuthFile(t, authDir, "antigravity-b.json", map[string]any{"type": "agy", "home": homeB, "email": "b@example.com"})
	modelsB := discoverModels(cfg)
	if !hasModelID(modelsB, "agy/model-b-one") {
		t.Fatalf("account B models missing after the account set changed: %#v", modelIDs(modelsB))
	}
	if !hasModelID(modelsB, "agy/model-a-one") {
		t.Fatalf("merged catalog dropped account A models: %#v", modelIDs(modelsB))
	}
}

func TestDiscoveryAccountsSkipsDisabledAndSignedOut(t *testing.T) {
	invalidateModels()
	signedIn := signedInHome(t)
	signedOut := t.TempDir()
	authDir := t.TempDir()
	writeAuthFile(t, authDir, "antigravity-disabled.json", map[string]any{"type": "agy", "home": signedIn, "disabled": true})
	writeAuthFile(t, authDir, "antigravity-signed-out.json", map[string]any{"type": "agy", "home": signedOut})
	setAuthDir(authDir)
	t.Cleanup(func() {
		setAuthDir("")
		invalidateModels()
	})

	accounts := discoveryAccounts(defaultConfig())
	for _, account := range accounts {
		if account.Home == signedIn {
			t.Fatalf("disabled account %q was selected for discovery", account.Home)
		}
		if account.Home == signedOut {
			t.Fatalf("signed-out home %q was selected for discovery", account.Home)
		}
	}
}

func TestParseAgyModelLinesHandlesGeminiClaudeAndGPTOss(t *testing.T) {
	models, mapping := parseAgyModelLines(`gemini-3.8-flash-high
gemini-3.8-flash-medium
gemini-3.8-flash-low
claude-sonnet-4-6
claude-opus-4-6-thinking
gpt-oss-120b-medium
`)
	wantIDs := map[string]bool{
		"agy/gemini-3.8-flash-high":    false,
		"agy/gemini-3.8-flash-medium":  false,
		"agy/gemini-3.8-flash-low":     false,
		"agy/claude-sonnet-4-6":        false,
		"agy/claude-opus-4-6-thinking": false,
		"agy/gpt-oss-120b-medium":      false,
	}
	for _, model := range models {
		if _, ok := wantIDs[model.ID]; ok {
			wantIDs[model.ID] = true
		}
	}
	for id, found := range wantIDs {
		if !found {
			t.Fatalf("model %q missing from %#v", id, modelIDs(models))
		}
	}
	if mapping["agy/claude-opus-4-6-thinking"] != "claude-opus-4-6-thinking" {
		t.Fatalf("unexpected native mapping: %#v", mapping)
	}
	if mapping["agy/gpt-oss-120b-medium"] != "gpt-oss-120b-medium" {
		t.Fatalf("unexpected native mapping: %#v", mapping)
	}
}

func countLines(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return count
}
