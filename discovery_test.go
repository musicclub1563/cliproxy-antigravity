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
	"sort"
	"strings"
	"testing"
)

// The six models a signed-in account reports on the reported setup. Tests never talk to a real
// account: every CLI call goes to a fake binary built from the sources below.
var realAgyModels = []string{
	"gemini-3.8-flash-high",
	"gemini-3.8-flash-medium",
	"gemini-3.8-flash-low",
	"claude-sonnet-4-6",
	"claude-opus-4-6-thinking",
	"gpt-oss-120b-medium",
}

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

// isolatedConfig points discovery at a path that cannot start, so a unit test never runs a real
// agy installation that may be present on the developer machine.
func isolatedConfig(t *testing.T) pluginConfig {
	t.Helper()
	cfg := defaultConfig()
	cfg.BinaryPath = filepath.Join(t.TempDir(), fakeBinaryName())
	return cfg
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

const fakeAgyEmptySuccessSource = `package main

import "os"

func main() {
	if len(os.Args) < 2 || os.Args[1] != "models" {
		os.Exit(2)
	}
	os.Exit(0)
}
`

const fakeAgySameModelsSource = `package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "models" {
		os.Exit(2)
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

const fakeAgyModelFileSource = `package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "models" {
		os.Exit(2)
	}
	data, err := os.ReadFile(os.Getenv("FAKE_AGY_MODELS_FILE"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "missing model file")
		os.Exit(1)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			fmt.Println(trimmed)
		}
	}
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

// setupAccountAuth registers a single signed-in account the way the accounts page does.
func setupAccountAuth(t *testing.T, home string) string {
	t.Helper()
	authDir := t.TempDir()
	writeAuthFile(t, authDir, "antigravity-user@example.com.json", map[string]any{
		"type":     "agy",
		"email":    "user@example.com",
		"home":     home,
		"disabled": false,
	})
	setAuthDir(authDir)
	t.Cleanup(func() {
		setAuthDir("")
		invalidateModels()
	})
	return authDir
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

func TestDiscoverModelsPublishesOnlyRealModels(t *testing.T) {
	invalidateModels()
	accountHome := signedInHome(t)
	setupAccountAuth(t, accountHome)

	cfg := defaultConfig()
	cfg.BinaryPath = buildFakeAgy(t, fmt.Sprintf(fakeAgyHomeGateSource, accountHome))

	models := discoverModels(cfg)
	got := modelIDs(models)
	want := append([]string(nil), realAgyModels...)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("published models = %#v, want exactly %#v", got, want)
	}

	// Nothing extra: no prefixed duplicates, no effort-less base models, no default entry.
	for _, id := range got {
		if strings.HasPrefix(strings.ToLower(id), "agy/") || strings.HasPrefix(strings.ToLower(id), "antigravity/") {
			t.Fatalf("prefixed duplicate %q was published", id)
		}
	}
	for _, shadow := range []string{"gemini-3.8-flash", "gpt-oss-120b", "claude-opus-4-6", "agy/default", "default"} {
		if hasModelID(models, shadow) {
			t.Fatalf("compatibility candidate %q must not be published: %#v", shadow, got)
		}
	}

	// Aliases still resolve, they are just not listed.
	aliases := map[string]string{
		"agy/claude-sonnet-4-6":         "claude-sonnet-4-6",
		"antigravity/claude-sonnet-4-6": "claude-sonnet-4-6",
		"Claude Sonnet 4.6":             "claude-sonnet-4-6",
		"agy/gpt-oss-120b-medium":       "gpt-oss-120b-medium",
		"gpt-oss-120b":                  "gpt-oss-120b",
		"agy/default":                   "",
		"default":                       "",
		"agy/gemini-3.8-flash-high":     "gemini-3.8-flash-high",
		"gemini-3.8-flash":              "gemini-3.8-flash",
	}
	for request, native := range aliases {
		if gotNative := resolveNativeModel(request, cfg); gotNative != native {
			t.Fatalf("resolveNativeModel(%q) = %q, want %q", request, gotNative, native)
		}
	}

	// Published IDs and legacy spellings both route to this plugin.
	for _, request := range []string{
		"claude-sonnet-4-6",
		"agy/claude-sonnet-4-6",
		"antigravity/claude-sonnet-4-6",
		"gpt-oss-120b",
		"gpt-oss-120b-medium",
	} {
		resp := routeModel(modelRouteRequest{RequestedModel: request})
		if !resp.Handled || resp.TargetKind != "self" {
			t.Fatalf("routeModel(%q) = %+v, want a self route", request, resp)
		}
	}
}

func TestDiscoverModelsPublishesEmptyListOnEmptySuccess(t *testing.T) {
	invalidateModels()
	setupAccountAuth(t, signedInHome(t))

	cfg := defaultConfig()
	cfg.BinaryPath = buildFakeAgy(t, fakeAgyEmptySuccessSource)

	models := discoverModels(cfg)
	if len(models) != 0 {
		t.Fatalf("empty successful discovery published %#v, want nothing", modelIDs(models))
	}
	if modelCatalog.fallback {
		t.Fatal("empty successful discovery must not use the built-in fallback")
	}
	if !strings.Contains(modelCatalog.diagnostic, "empty list") {
		t.Fatalf("expected an empty-list diagnostic, got %q", modelCatalog.diagnostic)
	}
}

func TestDiscoverModelsFallsBackOnlyWhenCommandFails(t *testing.T) {
	invalidateModels()
	setupAccountAuth(t, signedInHome(t))

	cfg := defaultConfig()
	cfg.BinaryPath = buildFakeAgy(t, fakeAgyFailingSource)

	models := discoverModels(cfg)
	if !hasModelID(models, "agy/default") {
		t.Fatalf("failure fallback is missing: %#v", modelIDs(models))
	}
	if !modelCatalog.fallback {
		t.Fatal("expected the fallback flag on a failed discovery")
	}
	if hasModelID(models, "evil-model-one") || hasModelID(models, "evil-model-two") {
		t.Fatalf("stdout of a failed agy run was parsed as models: %#v", modelIDs(models))
	}

	diagnostic := modelCatalog.diagnostic
	if !strings.Contains(diagnostic, "failed model discovery") || !strings.Contains(diagnostic, "built-in fallback") {
		t.Fatalf("expected a fallback diagnostic, got %q", diagnostic)
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
	if !hasModelID(models, "legacy-model-one") {
		t.Fatalf("legacy discovery fallback did not run agy: %#v", modelIDs(models))
	}
}

// TestDiscoveryFollowsNewModelsWithoutHardcoding proves the catalog is driven by the CLI output:
// a model added to the same account shows up on the next discovery, with no code change.
func TestDiscoveryFollowsNewModelsWithoutHardcoding(t *testing.T) {
	invalidateModels()
	setupAccountAuth(t, signedInHome(t))

	modelsFile := filepath.Join(t.TempDir(), "models.txt")
	t.Setenv("FAKE_AGY_MODELS_FILE", modelsFile)
	writeModels := func(list []string) {
		t.Helper()
		if err := os.WriteFile(modelsFile, []byte(strings.Join(list, "\n")+"\n"), 0o600); err != nil {
			t.Fatalf("write model file: %v", err)
		}
	}

	writeModels(realAgyModels)
	cfg := defaultConfig()
	cfg.BinaryPath = buildFakeAgy(t, fakeAgyModelFileSource)

	first := discoverModels(cfg)
	if len(first) != len(realAgyModels) {
		t.Fatalf("initial catalog = %#v, want %d entries", modelIDs(first), len(realAgyModels))
	}

	// agy starts reporting one more model; the next (uncached) discovery publishes it as-is.
	writeModels(append(append([]string(nil), realAgyModels...), "gemini-3.9-flash-high"))
	invalidateModels()
	second := discoverModels(cfg)
	if len(second) != len(realAgyModels)+1 || !hasModelID(second, "gemini-3.9-flash-high") {
		t.Fatalf("catalog did not follow the new model: %#v", modelIDs(second))
	}
	if !catalogClaimable("gemini-3.9-flash-high") {
		t.Fatal("newly discovered model is not routable")
	}
}

func TestMultiAccountDiscoveryDedupesPublishedModels(t *testing.T) {
	invalidateModels()
	homeA := signedInHome(t)
	homeB := signedInHome(t)
	authDir := t.TempDir()
	writeAuthFile(t, authDir, "antigravity-a.json", map[string]any{"type": "agy", "home": homeA, "email": "a@example.com"})
	writeAuthFile(t, authDir, "antigravity-b.json", map[string]any{"type": "agy", "home": homeB, "email": "b@example.com"})
	setAuthDir(authDir)
	t.Cleanup(func() {
		setAuthDir("")
		invalidateModels()
	})

	cfg := defaultConfig()
	cfg.BinaryPath = buildFakeAgy(t, fakeAgySameModelsSource)

	models := discoverModels(cfg)
	got := modelIDs(models)
	want := append([]string(nil), realAgyModels...)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merged models = %#v, want the de-duplicated %#v", got, want)
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
	if !hasModelID(modelsA, "model-a-one") {
		t.Fatalf("account A models missing: %#v", modelIDs(modelsA))
	}
	if hasModelID(modelsA, "model-b-one") {
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
	if !hasModelID(modelsB, "model-b-one") {
		t.Fatalf("account B models missing after the account set changed: %#v", modelIDs(modelsB))
	}
	if !hasModelID(modelsB, "model-a-one") {
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

func TestParseAgyModelOutputKeepsOnlyReportedLines(t *testing.T) {
	models := parseAgyModelOutput(`gemini-3.8-flash-high
gemini-3.8-flash-medium
gemini-3.8-flash-low
claude-sonnet-4-6
claude-opus-4-6-thinking
gpt-oss-120b-medium
`)
	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ID)
		if model.Native != model.ID {
			t.Fatalf("native = %q for slug line %q", model.Native, model.ID)
		}
	}
	want := append([]string(nil), realAgyModels...)
	sort.Strings(want)
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("parsed IDs = %#v, want %#v", ids, want)
	}
}
