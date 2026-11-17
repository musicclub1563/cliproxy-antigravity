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

// Dynamic model discovery and catalog publication.
//
// Two things are deliberately kept apart:
//
//   - The published catalog: exactly the model IDs the signed-in accounts report. Nothing is
//     synthesised on top of a successful discovery, so a client shows one entry per real model.
//   - The alias table: legacy request names (agy/..., antigravity/..., the humanised label and
//     the effort-less base name) still resolve to the same native model, but they are never
//     published as separate entries.
//
// `agy models` reads the login of the HOME it runs under, so discovery runs once per
// signed-in account home; running it from CLIProxyAPI's own HOME (the service user) makes agy
// answer "Please sign in to view available models".
const (
	// modelsCommandTimeout bounds one `agy models` run.
	modelsCommandTimeout = 20 * time.Second
	// modelCatalogTTL keeps the merged catalog briefly so the request path does not respawn agy.
	modelCatalogTTL = time.Minute
)

// agyModel pairs the ID published to CLIProxyAPI with the name passed to `agy --model`.
// They differ only for older CLIs that print human-readable labels instead of slugs.
type agyModel struct {
	ID     string
	Native string
}

type catalogState struct {
	sync.Mutex
	// key identifies the inputs (binary, workdir, account homes, account token mtimes) whose
	// result is cached, so one account's list is never served for another.
	key       string
	loaded    bool
	fetchedAt time.Time
	// models is the published catalog. It only ever contains real discovered models, plus the
	// built-in fallback when discovery failed for every account.
	models []modelInfo
	// nativeByID maps lowercase request names (published IDs and legacy aliases) to the native
	// agy model name. Aliases are resolution-only and never published.
	nativeByID map[string]string
	// claimable holds the lowercase bare model names the router may claim without a prefix.
	claimable map[string]bool
	// fallback reports whether models came from the built-in list instead of a live discovery.
	fallback bool
	// diagnostic is the last sanitized discovery message; it is only logged when it changes.
	diagnostic string
}

var modelCatalog = catalogState{}

func invalidateModels() {
	modelCatalog.Lock()
	modelCatalog.key = ""
	modelCatalog.loaded = false
	modelCatalog.fetchedAt = time.Time{}
	modelCatalog.models = nil
	modelCatalog.nativeByID = nil
	modelCatalog.claimable = nil
	modelCatalog.fallback = false
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

// discoverModels returns the published catalog.
//
// The built-in list is only used when no account could be queried at all: a successful run,
// including one that reports no models, is published as-is so the client never sees entries the
// CLI did not report.
func discoverModels(cfg pluginConfig) []modelInfo {
	accounts := discoveryAccounts(cfg)
	key := catalogKey(cfg, accounts)

	modelCatalog.Lock()
	defer modelCatalog.Unlock()

	if modelCatalog.loaded && modelCatalog.key == key && time.Since(modelCatalog.fetchedAt) < modelCatalogTTL {
		return cloneModels(modelCatalog.models)
	}

	// A single-account install may have no agy auth file at all: agy then reads the login stored
	// under CLIProxyAPI's own HOME, which is what discovery always did before this change.
	targets := accounts
	legacy := false
	if len(targets) == 0 {
		legacy = true
		targets = []discoveryAccount{{Label: "process environment"}}
	}

	published := make([]modelInfo, 0, 8)
	seen := make(map[string]bool, 8)
	nativeByID := defaultModelAliases()
	claimable := make(map[string]bool, 8)
	failures := make([]string, 0, len(targets))
	successes := 0

	for _, account := range targets {
		models, err := fetchModelsForAccount(cfg, account.Home)
		if err != nil {
			// Account labels are user emails; sanitize both halves before they reach a log.
			failures = append(failures, sanitizeDiagnostic(account.Label)+": "+sanitizeDiagnostic(err.Error()))
			continue
		}
		successes++
		for _, model := range models {
			modelID := strings.TrimSpace(model.ID)
			if modelID == "" {
				continue
			}
			// Published IDs keep the provider prefix so the client groups them under "agy";
			// only the display name drops it.
			key := strings.ToLower(publishedModelID(modelID))
			if !seen[key] {
				seen[key] = true
				published = append(published, publishedModelInfo(modelID))
			}
			registerModelAliases(nativeByID, model)
			claimable[strings.ToLower(modelID)] = true
			// The effort-less base name stays resolvable, and claimable for older clients,
			// but is not published as another model.
			if base := strings.TrimSpace(stripEffortSuffix(modelID)); base != "" && base != modelID {
				claimable[strings.ToLower(base)] = true
			}
		}
	}

	fallback := successes == 0
	if fallback {
		// Discovery is broken: keep the previous compatibility list, and only here.
		published = standardBuiltinModels()
		nativeByID = builtinModelAliases()
		claimable = builtinClaimableModels()
	}

	sort.SliceStable(published, func(i, j int) bool { return published[i].ID < published[j].ID })

	modelCatalog.key = key
	modelCatalog.loaded = true
	modelCatalog.fetchedAt = time.Now()
	modelCatalog.models = published
	modelCatalog.nativeByID = nativeByID
	modelCatalog.claimable = claimable
	modelCatalog.fallback = fallback
	modelCatalog.diagnostic = logDiscoveryOutcome(len(accounts), legacy, len(published), fallback, failures)
	return cloneModels(published)
}

// publishedModelID is the ID exposed to CLIProxyAPI: the native model name under the provider
// prefix, so clients keep grouping agy models together.
func publishedModelID(nativeModel string) string {
	return providerID + "/" + strings.TrimSpace(nativeModel)
}

// publishedModelInfo builds the catalog entry for one discovered model ID. The display name is
// derived from the native name only, so it never carries a provider prefix.
func publishedModelInfo(modelID string) modelInfo {
	name := humanizeModelName(modelID)
	if name == "" {
		name = modelID
	}
	return modelInfo{
		ID:                         publishedModelID(modelID),
		Object:                     "model",
		OwnedBy:                    "google-antigravity-cli",
		DisplayName:                name,
		Name:                       name,
		Description:                "Discovered from the installed official agy CLI.",
		SupportedGenerationMethods: []string{"chat"},
		UserDefined:                true,
	}
}

// registerModelAliases records every legacy spelling of a model without publishing it.
func registerModelAliases(aliases map[string]string, model agyModel) {
	native := strings.TrimSpace(model.Native)
	if native == "" {
		native = strings.TrimSpace(model.ID)
	}
	add := func(name, target string) {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" || strings.TrimSpace(target) == "" {
			return
		}
		if _, exists := aliases[name]; exists {
			return
		}
		aliases[name] = target
	}

	add(model.ID, native)
	add("agy/"+model.ID, native)
	add("antigravity/"+model.ID, native)
	add(humanizeModelName(model.ID), native)

	base := strings.TrimSpace(stripEffortSuffix(model.ID))
	if base != "" && base != model.ID {
		add(base, base)
		add("agy/"+base, base)
		add("antigravity/"+base, base)
	}
}

// defaultModelAliases keeps the "default model" request paths working without publishing them.
func defaultModelAliases() map[string]string {
	return map[string]string{
		"default":                         "",
		"agy/default":                     "",
		"antigravity/default":             "",
		"antigravity cli (default model)": "",
	}
}

// builtinModelAliases is the alias table that belongs to the fallback catalog.
func builtinModelAliases() map[string]string {
	aliases := defaultModelAliases()
	for id, native := range standardNativeModelMapping() {
		key := strings.ToLower(strings.TrimSpace(id))
		if key == "" {
			continue
		}
		if _, exists := aliases[key]; !exists {
			aliases[key] = native
		}
	}
	return aliases
}

// builtinClaimableModels holds the fallback IDs the router may claim. The default model stays
// request-only because a bare "default" is too generic to intercept.
func builtinClaimableModels() map[string]bool {
	claimable := make(map[string]bool, 8)
	for _, model := range standardBuiltinModels() {
		if strings.EqualFold(model.ID, "agy/default") {
			continue
		}
		claimable[strings.ToLower(model.ID)] = true
	}
	return claimable
}

// catalogClaimable reports whether the router may claim a bare model name. Before the first
// discovery the built-in set is used so a request that races plugin startup still works.
func catalogClaimable(model string) bool {
	modelCatalog.Lock()
	defer modelCatalog.Unlock()
	key := strings.ToLower(strings.TrimSpace(model))
	if key == "" {
		return false
	}
	if !modelCatalog.loaded {
		return builtinClaimableModels()[key]
	}
	return modelCatalog.claimable[key]
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
func fetchModelsForAccount(cfg pluginConfig, home string) ([]agyModel, error) {
	accountCfg, err := withAccount(cfg, map[string]any{"home": home})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), modelsCommandTimeout)
	defer cancel()

	cmd, err := newModelsCommand(ctx, accountCfg)
	if err != nil {
		return nil, err
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if errStart := cmd.Start(); errStart != nil {
		return nil, fmt.Errorf("could not start agy models: %w", errStart)
	}
	activeProcesses.add(cmd)
	errWait := cmd.Wait()
	activeProcesses.remove(cmd)

	if ctxErr := ctx.Err(); ctxErr == context.DeadlineExceeded {
		return nil, fmt.Errorf("agy models timed out after %s", modelsCommandTimeout)
	}
	if errWait != nil {
		// Error output never becomes a model: only stdout of a successful run is parsed.
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = errWait.Error()
		}
		return nil, fmt.Errorf("%s", message)
	}

	return parseAgyModelOutput(stdout.String()), nil
}

// parseAgyModelOutput reads `agy models` output into publishable model IDs plus their native
// names. One line is one model: no base variants and no effort aliases are synthesised here.
func parseAgyModelOutput(output string) []agyModel {
	models := make([]agyModel, 0, 8)
	seen := make(map[string]bool, 8)

	for _, rawLine := range strings.Split(output, "\n") {
		line := strings.TrimSpace(rawLine)
		line = strings.TrimSpace(strings.TrimLeft(line, "*•-"))
		if line == "" {
			continue
		}

		id := ""
		native := ""
		if parts := strings.SplitN(line, "\t", 2); len(parts) == 2 {
			id = strings.TrimSpace(parts[0])
			native = id
		} else if fields := strings.Fields(line); len(fields) > 0 && looksLikeModelSlug(fields[0]) {
			id = fields[0]
			native = id
		} else {
			// Older builds print a human label only; keep the label as the native name.
			native = line
			id = slugifyModelLabel(line)
		}

		id = trimModelProviderPrefix(id)
		if id == "" || native == "" {
			continue
		}
		key := strings.ToLower(id)
		if seen[key] {
			continue
		}
		seen[key] = true
		models = append(models, agyModel{ID: id, Native: native})
	}

	sort.SliceStable(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return models
}

func trimModelProviderPrefix(name string) string {
	trimmed := strings.TrimSpace(name)
	lower := strings.ToLower(trimmed)
	for _, prefix := range []string{"agy/", "antigravity/"} {
		if strings.HasPrefix(lower, prefix) {
			return strings.TrimSpace(trimmed[len(prefix):])
		}
	}
	return trimmed
}

// logDiscoveryOutcome records a sanitized diagnostic line, and only when it changes.
func logDiscoveryOutcome(accounts int, legacy bool, published int, fallback bool, failures []string) string {
	if len(failures) == 0 {
		if published == 0 {
			const message = "agy reported no models for the signed-in accounts; publishing an empty list"
			if modelCatalog.diagnostic != message {
				log.Printf("[%s] %s", pluginID, message)
			}
			return message
		}
		if modelCatalog.diagnostic != "" {
			log.Printf("[%s] model discovery recovered (%d account(s), %d model(s))", pluginID, accounts, published)
		}
		return ""
	}
	scope := fmt.Sprintf("%d/%d account(s)", len(failures), accounts)
	if legacy {
		scope = "process environment"
	}
	message := fmt.Sprintf("%s failed model discovery: %s", scope, strings.Join(failures, "; "))
	if fallback {
		message += "; serving the built-in fallback list"
	}
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
