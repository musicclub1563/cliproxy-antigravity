package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Dynamic model discovery.
//
// `agy models` reads the login of the HOME it runs under, so discovery has to run it once per
// signed-in account home. Running it from CLIProxyAPI's own HOME (the service user) makes agy
// answer "Please sign in to view available models", which used to leave the catalog with only the
// built-in Gemini entries.
const (
	// modelsCommandTimeout bounds one `agy models` run.
	modelsCommandTimeout = 20 * time.Second
	// modelCatalogTTL keeps the merged catalog briefly so the request path does not respawn agy.
	modelCatalogTTL = time.Minute
)

type catalogState struct {
	sync.Mutex
	// key identifies the inputs (binary, workdir, account homes, account token mtimes) whose
	// result is cached, so one account's list is never served for another.
	key        string
	fetchedAt  time.Time
	models     []modelInfo
	nativeByID map[string]string
	// diagnostic is the last sanitized discovery failure; it is only logged when it changes.
	diagnostic string
}

var modelCatalog = catalogState{}

func invalidateModels() {
	modelCatalog.Lock()
	modelCatalog.key = ""
	modelCatalog.fetchedAt = time.Time{}
	modelCatalog.models = nil
	modelCatalog.nativeByID = nil
	modelCatalog.diagnostic = ""
	modelCatalog.Unlock()
}

// discoveryAccount is one agy home that can answer `agy models`.
type discoveryAccount struct {
	Home  string
	Label string
}

// discoveryAccounts returns the enabled, signed-in account homes to discover models from.
// It reuses the account metadata CLIProxyAPI already receives through the plugin auth files
// (type "agy" plus the home written by the accounts page) and, as a safety net, the signed-in
// homes under accounts_dir. The process home is only a fallback for single-account installs.
func discoveryAccounts(cfg pluginConfig) []discoveryAccount {
	accounts := make([]discoveryAccount, 0, 4)
	seen := make(map[string]bool, 4)
	add := func(home, label string) {
		if strings.TrimSpace(home) == "" {
			return
		}
		validated, err := validateWorkdir(home)
		if err != nil || validated == "" {
			return
		}
		if seen[validated] || !accountSignedIn(validated) {
			return
		}
		seen[validated] = true
		accounts = append(accounts, discoveryAccount{Home: validated, Label: label})
	}

	processHome, _ := os.UserHomeDir()
	for _, record := range agyAuthRecords(currentAuthDir()) {
		if record.Disabled {
			continue
		}
		// Auth files without a home describe the account agy keeps in the process home.
		if record.Home == "" {
			add(processHome, record.Email)
			continue
		}
		add(record.Home, record.Email)
	}

	if cfg.AccountsDir != "" {
		if entries, err := os.ReadDir(cfg.AccountsDir); err == nil {
			for _, entry := range entries {
				if entry.IsDir() {
					add(filepath.Join(cfg.AccountsDir, entry.Name()), entry.Name())
				}
			}
		}
	}

	add(processHome, "process-home")

	sort.Slice(accounts, func(i, j int) bool {
		return accounts[i].Home < accounts[j].Home
	})
	return accounts
}

// agyAuthRecord is the subset of an agy auth file that model discovery needs.
type agyAuthRecord struct {
	FileName string
	Email    string
	Home     string
	Disabled bool
}

// agyAuthRecords reads the agy auth files CLIProxyAPI has registered.
func agyAuthRecords(authDir string) []agyAuthRecord {
	authDir = strings.TrimSpace(authDir)
	if authDir == "" {
		return nil
	}
	entries, err := os.ReadDir(authDir)
	if err != nil {
		return nil
	}
	records := make([]agyAuthRecord, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(strings.ToLower(name), ".json") {
			continue
		}
		data, errRead := os.ReadFile(filepath.Join(authDir, name))
		if errRead != nil || len(bytes.TrimSpace(data)) == 0 {
			continue
		}
		var meta struct {
			Type     string `json:"type"`
			Email    string `json:"email"`
			Label    string `json:"label"`
			Home     string `json:"home"`
			Disabled bool   `json:"disabled"`
		}
		if errJSON := json.Unmarshal(data, &meta); errJSON != nil {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(meta.Type), providerID) {
			continue
		}
		label := strings.TrimSpace(meta.Email)
		if label == "" {
			label = strings.TrimSpace(meta.Label)
		}
		if label == "" {
			label = name
		}
		records = append(records, agyAuthRecord{
			FileName: name,
			Email:    label,
			Home:     strings.TrimSpace(meta.Home),
			Disabled: meta.Disabled,
		})
	}
	sort.Slice(records, func(i, j int) bool { return records[i].FileName < records[j].FileName })
	return records
}

// discoverModels returns the merged model catalog: the built-in Gemini entries first so an
// unreachable or signed-out account still keeps the previous behavior, then every model the
// signed-in accounts report.
func discoverModels(cfg pluginConfig) []modelInfo {
	accounts := discoveryAccounts(cfg)
	key := catalogKey(cfg, accounts)

	modelCatalog.Lock()
	defer modelCatalog.Unlock()

	if modelCatalog.key == key && len(modelCatalog.models) > 0 && time.Since(modelCatalog.fetchedAt) < modelCatalogTTL {
		return cloneModels(modelCatalog.models)
	}

	models := append([]modelInfo(nil), standardBuiltinModels()...)
	nativeByID := standardNativeModelMapping()
	seenModels := make(map[string]bool, len(models)+8)
	for _, model := range models {
		seenModels[strings.ToLower(model.ID)] = true
	}

	// A single-account install may have no agy auth file at all: agy then reads the login stored
	// under CLIProxyAPI's own HOME, which is what discovery always did before this change.
	targets := accounts
	legacy := false
	if len(targets) == 0 {
		legacy = true
		targets = []discoveryAccount{{Label: "process environment"}}
	}

	failures := make([]string, 0, len(targets))
	discoveredCount := 0
	for _, account := range targets {
		accountModels, accountMapping, err := fetchModelsForAccount(cfg, account.Home)
		if err != nil {
			// Account labels are user emails; sanitize both halves before they reach a log.
			failures = append(failures, sanitizeDiagnostic(account.Label)+": "+sanitizeDiagnostic(err.Error()))
			continue
		}
		discoveredCount += len(accountModels)
		for _, model := range accountModels {
			key := strings.ToLower(model.ID)
			if seenModels[key] {
				continue
			}
			seenModels[key] = true
			models = append(models, model)
		}
		for id, native := range accountMapping {
			if _, exists := nativeByID[id]; !exists {
				nativeByID[id] = native
			}
		}
	}

	modelCatalog.key = key
	modelCatalog.fetchedAt = time.Now()
	modelCatalog.models = models
	modelCatalog.nativeByID = nativeByID
	modelCatalog.diagnostic = logDiscoveryOutcome(len(accounts), legacy, discoveredCount, failures)
	return cloneModels(models)
}

// catalogKey builds the cache identity. Account token mtimes are included so re-signing in an
// account refreshes the catalog without waiting for the TTL.
func catalogKey(cfg pluginConfig, accounts []discoveryAccount) string {
	parts := make([]string, 0, len(accounts)*2+2)
	parts = append(parts, cfg.BinaryPath, cfg.Workdir)
	if len(accounts) == 0 {
		parts = append(parts, "<no-signed-in-account>")
	}
	for _, account := range accounts {
		parts = append(parts, account.Home)
		if info, err := os.Stat(filepath.Join(agyStateDir(account.Home), "antigravity-oauth-token")); err == nil {
			parts = append(parts, strconv.FormatInt(info.ModTime().UnixNano(), 10))
		} else {
			parts = append(parts, "-")
		}
	}
	return strings.Join(parts, "\x00")
}

// newModelsCommand builds the `agy models` command bound to an account environment.
func newModelsCommand(ctx context.Context, cfg pluginConfig) (*exec.Cmd, error) {
	if err := validateBinaryPath(cfg.BinaryPath); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, resolveBinaryPath(cfg.BinaryPath), "models")
	if cfg.Workdir != "" {
		cmd.Dir = cfg.Workdir
	}
	// Only the child gets the account home; the host process environment is untouched.
	applyAccountEnv(cmd, cfg)
	configureSysProcAttr(cmd)
	return cmd, nil
}

// fetchModelsForAccount runs `agy models` with the account's HOME and parses a successful run.
func fetchModelsForAccount(cfg pluginConfig, home string) ([]modelInfo, map[string]string, error) {
	accountCfg, err := withAccount(cfg, map[string]any{"home": home})
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), modelsCommandTimeout)
	defer cancel()

	cmd, err := newModelsCommand(ctx, accountCfg)
	if err != nil {
		return nil, nil, err
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if errStart := cmd.Start(); errStart != nil {
		return nil, nil, fmt.Errorf("could not start agy models: %w", errStart)
	}
	activeProcesses.add(cmd)
	errWait := cmd.Wait()
	activeProcesses.remove(cmd)

	if ctxErr := ctx.Err(); ctxErr == context.DeadlineExceeded {
		return nil, nil, fmt.Errorf("agy models timed out after %s", modelsCommandTimeout)
	}
	if errWait != nil {
		// Error output never becomes a model: only stdout of a successful run is parsed.
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = errWait.Error()
		}
		return nil, nil, fmt.Errorf("%s", message)
	}

	models, mapping := parseAgyModelLines(stdout.String())
	return models, mapping, nil
}

// logDiscoveryOutcome records a sanitized diagnostic line, and only when it changes.
func logDiscoveryOutcome(accounts int, legacy bool, discovered int, failures []string) string {
	if len(failures) == 0 {
		if modelCatalog.diagnostic != "" {
			log.Printf("[%s] model discovery recovered (%d account(s), %d discovered model(s))", pluginID, accounts, discovered)
		}
		return ""
	}
	scope := fmt.Sprintf("%d/%d account(s)", len(failures), accounts)
	if legacy {
		scope = "process environment"
	}
	message := fmt.Sprintf("%s failed model discovery: %s", scope, strings.Join(failures, "; "))
	if modelCatalog.diagnostic != message {
		log.Printf("[%s] %s", pluginID, message)
	}
	return message
}

var (
	emailPattern     = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	longTokenPattern = regexp.MustCompile(`[A-Za-z0-9_\-\.]{32,}`)
)

// sanitizeDiagnostic strips credentials and personal data from agy output before logging it.
func sanitizeDiagnostic(text string) string {
	trimmed := strings.TrimSpace(text)
	if index := strings.IndexByte(trimmed, '\n'); index >= 0 {
		trimmed = strings.TrimSpace(trimmed[:index])
	}
	trimmed = emailPattern.ReplaceAllString(trimmed, "<email>")
	trimmed = longTokenPattern.ReplaceAllString(trimmed, "<redacted>")
	if len(trimmed) > 240 {
		trimmed = trimmed[:240] + "..."
	}
	return trimmed
}

// standardNativeModelMapping maps the built-in aliases to the native agy model names.
func standardNativeModelMapping() map[string]string {
	return map[string]string{
		"agy/default":                     "",
		"antigravity/default":             "",
		"default":                         "",
		"Antigravity CLI (default model)": "",
		"gemini-3.8-flash":                "gemini-3.8-flash",
		"Gemini 3.8 Flash":                "gemini-3.8-flash",
		"gemini-3.8-flash-high":           "gemini-3.8-flash-high",
		"Gemini 3.8 Flash (High)":         "gemini-3.8-flash-high",
		"gemini-3.8-flash-medium":         "gemini-3.8-flash-medium",
		"Gemini 3.8 Flash (Medium)":       "gemini-3.8-flash-medium",
		"gemini-3.8-flash-low":            "gemini-3.8-flash-low",
		"Gemini 3.8 Flash (Low)":          "gemini-3.8-flash-low",
	}
}
