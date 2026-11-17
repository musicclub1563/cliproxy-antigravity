package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"testing"
)

func TestParseAgyModelOutputPublishesOneEntryPerLine(t *testing.T) {
	models := parseAgyModelOutput(`
gemini-3.6-flash-high	Gemini 3.6 Flash (High)
gemini-3.6-flash-medium    Gemini 3.6 Flash (Medium)
* Claude Sonnet 4.6 (Thinking)
gemini-3.6-flash-high
`)
	if len(models) != 3 {
		t.Fatalf("len(models) = %d, want 3 (one per line, de-duplicated): %#v", len(models), models)
	}
	byID := map[string]agyModel{}
	for _, m := range models {
		byID[m.ID] = m
	}
	if byID["gemini-3.6-flash-high"].Native != "gemini-3.6-flash-high" {
		t.Fatalf("unexpected native for tab-separated line: %#v", byID["gemini-3.6-flash-high"])
	}
	if byID["claude-sonnet-4-6-thinking"].Native != "Claude Sonnet 4.6 (Thinking)" {
		t.Fatalf("label-only line must keep the label as native: %#v", byID["claude-sonnet-4-6-thinking"])
	}
	// Base variants are aliases only, never published entries.
	if _, exists := byID["gemini-3.6-flash"]; exists {
		t.Fatalf("effort-less base model was parsed as a model: %#v", models)
	}

	raw := parseAgyModelOutput(`
gemini-3.8-flash-high
gpt-oss-120b-medium
claude-sonnet-4-6
`)
	rawIDs := make([]string, 0, len(raw))
	for _, m := range raw {
		rawIDs = append(rawIDs, m.ID)
	}
	want := []string{"claude-sonnet-4-6", "gemini-3.8-flash-high", "gpt-oss-120b-medium"}
	if !reflect.DeepEqual(rawIDs, want) {
		t.Fatalf("raw parsed IDs = %#v, want %#v", rawIDs, want)
	}
}

func TestHumanizeModelName(t *testing.T) {
	tests := map[string]string{
		"gemini-3.8-flash-high":    "Gemini 3.8 Flash (High)",
		"gemini-3.8-flash":         "Gemini 3.8 Flash",
		"gpt-oss-120b-medium":      "GPT-OSS 120B (Medium)",
		"gpt-oss-120b":             "GPT-OSS 120B",
		"claude-sonnet-4-6":        "Claude Sonnet 4.6",
		"claude-opus-4-6-thinking": "Claude Opus 4.6 (Thinking)",
		"default":                  "Antigravity CLI (default model)",
	}
	for slug, want := range tests {
		got := humanizeModelName(slug)
		if got != want {
			t.Errorf("humanizeModelName(%q) = %q, want %q", slug, got, want)
		}
	}
}

func TestStandardBuiltinModelsHaveCleanNames(t *testing.T) {
	for _, m := range standardBuiltinModels() {
		if m.Name == "" || looksLikeModelSlug(m.Name) {
			t.Fatalf("builtin model %s has ugly slug name %q", m.ID, m.Name)
		}
	}
}

func TestAgyArgs(t *testing.T) {
	cfg := defaultConfig()
	cfg.DangerouslySkipPermissions = true
	cfg.Sandbox = true
	opts := agyOptions{
		NativeModel:     "gemini-3.8-flash",
		OutputFormat:    "stream-json",
		ConversationID:  "test-conv-123",
		ReasoningEffort: "high",
	}
	got := agyArgs(cfg, opts)
	want := []string{
		"--output-format", "stream-json",
		"--print-timeout", "30m",
		"--disable-slash-commands",
		"--dangerously-skip-permissions",
		"--sandbox",
		"--model", "gemini-3.8-flash",
		"--conversation", "test-conv-123",
		"--effort", "high",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("agyArgs() = %#v, want %#v", got, want)
	}
}

func TestAgyArgsResolvesConflict(t *testing.T) {
	cfg := defaultConfig()
	opts := agyOptions{
		NativeModel:     "gemini-3.8-flash-high",
		ReasoningEffort: "low",
	}
	got := agyArgs(cfg, opts)
	want := []string{
		"--output-format", "json",
		"--print-timeout", "30m",
		"--disable-slash-commands",
		"--model", "gemini-3.8-flash",
		"--effort", "low",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("agyArgs() = %#v, want %#v", got, want)
	}
}

func TestAgyArgsClaudeOmitsEffort(t *testing.T) {
	cfg := defaultConfig()
	opts := agyOptions{
		NativeModel:     "claude-sonnet-4-6",
		ReasoningEffort: "high",
	}
	got := agyArgs(cfg, opts)
	want := []string{
		"--output-format", "json",
		"--print-timeout", "30m",
		"--disable-slash-commands",
		"--model", "claude-sonnet-4-6",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("agyArgs() = %#v, want %#v", got, want)
	}
}

func TestResolveExecutionOptions(t *testing.T) {
	cfg := isolatedConfig(t)
	cfg.DefaultReasoningEffort = "low"

	// 1. From payload JSON
	payload, _ := json.Marshal(map[string]any{
		"model":            "agy/default",
		"reasoning_effort": "medium",
		"conversation_id":  "conv-from-payload",
	})
	req := rpcExecutorRequest{
		Model:   "agy/default",
		Payload: payload,
	}
	opts := resolveExecutionOptions(req, cfg)
	if opts.ReasoningEffort != "medium" {
		t.Fatalf("expected effort 'medium', got %q", opts.ReasoningEffort)
	}
	if opts.ConversationID != "conv-from-payload" {
		t.Fatalf("expected conversation 'conv-from-payload', got %q", opts.ConversationID)
	}

	// 2. From model suffix: e.g. agy/default:high
	req2 := rpcExecutorRequest{
		Model:   "agy/default:high",
		Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
	}
	opts2 := resolveExecutionOptions(req2, cfg)
	if opts2.ReasoningEffort != "high" {
		t.Fatalf("expected effort 'high' from model suffix, got %q", opts2.ReasoningEffort)
	}

	// 3. From headers
	headers := http.Header{}
	headers.Set("X-AGY-Conversation-ID", "conv-from-header")
	headers.Set("X-AGY-Effort", "low")
	req3 := rpcExecutorRequest{
		Model:   "agy/default",
		Headers: headers,
		Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
	}
	opts3 := resolveExecutionOptions(req3, cfg)
	if opts3.ConversationID != "conv-from-header" {
		t.Fatalf("expected conversation 'conv-from-header', got %q", opts3.ConversationID)
	}
	if opts3.ReasoningEffort != "low" {
		t.Fatalf("expected effort 'low', got %q", opts3.ReasoningEffort)
	}

	// 4. Built-in alias: antigravity/gemini-3.8-flash-high splits suffix into high effort and base model gemini-3.8-flash
	req4 := rpcExecutorRequest{
		Model:   "antigravity/gemini-3.8-flash-high",
		Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
	}
	opts4 := resolveExecutionOptions(req4, cfg)
	if opts4.ReasoningEffort != "high" {
		t.Fatalf("expected implicit high effort from gemini-3.8-flash-high, got %q", opts4.ReasoningEffort)
	}
	if opts4.NativeModel != "gemini-3.8-flash" {
		t.Fatalf("expected native model gemini-3.8-flash, got %q", opts4.NativeModel)
	}

	// 5. Explicit model alias (gemini-3.8-flash-high) splits into base model and high effort, ignoring payload low effort
	req5 := rpcExecutorRequest{
		Model:   "agy/gemini-3.8-flash-high",
		Payload: []byte(`{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"low"}`),
	}
	opts5 := resolveExecutionOptions(req5, cfg)
	if opts5.ReasoningEffort != "high" {
		t.Fatalf("expected model alias to take priority for effort, got %q", opts5.ReasoningEffort)
	}
	if opts5.NativeModel != "gemini-3.8-flash" {
		t.Fatalf("expected native model gemini-3.8-flash, got %q", opts5.NativeModel)
	}

	// 6. Colon override on model with effort suffix: gemini-3.8-flash-high:low overrides effort to low and strips -high
	req6 := rpcExecutorRequest{
		Model:   "agy/gemini-3.8-flash-high:low",
		Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
	}
	opts6 := resolveExecutionOptions(req6, cfg)
	if opts6.ReasoningEffort != "low" {
		t.Fatalf("expected colon override to set effort to 'low', got %q", opts6.ReasoningEffort)
	}
	if opts6.NativeModel != "gemini-3.8-flash" {
		t.Fatalf("expected native model gemini-3.8-flash after stripping conflicting suffix, got %q", opts6.NativeModel)
	}

	// 7. Base model gemini-3.8-flash with payload reasoning_effort
	req7 := rpcExecutorRequest{
		Model:   "agy/gemini-3.8-flash",
		Payload: []byte(`{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"low"}`),
	}
	opts7 := resolveExecutionOptions(req7, cfg)
	if opts7.ReasoningEffort != "low" {
		t.Fatalf("expected effort 'low' from payload, got %q", opts7.ReasoningEffort)
	}
	if opts7.NativeModel != "gemini-3.8-flash" {
		t.Fatalf("expected native model gemini-3.8-flash, got %q", opts7.NativeModel)
	}

	// 8. Base model without any effort provided falls back to model default (high)
	req8 := rpcExecutorRequest{
		Model:   "agy/gemini-3.8-flash",
		Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
	}
	opts8 := resolveExecutionOptions(req8, isolatedConfig(t))
	if opts8.ReasoningEffort != "high" {
		t.Fatalf("expected fallback effort 'high' for base gemini model, got %q", opts8.ReasoningEffort)
	}
	if opts8.NativeModel != "gemini-3.8-flash" {
		t.Fatalf("expected native model gemini-3.8-flash, got %q", opts8.NativeModel)
	}

	// 9. Claude model does not support effort and effort is cleared
	req9 := rpcExecutorRequest{
		Model:   "agy/claude-sonnet-4-6",
		Payload: []byte(`{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`),
	}
	opts9 := resolveExecutionOptions(req9, cfg)
	if opts9.ReasoningEffort != "" {
		t.Fatalf("expected effort to be cleared for claude model, got %q", opts9.ReasoningEffort)
	}
	if opts9.NativeModel != "claude-sonnet-4-6" {
		t.Fatalf("expected native model claude-sonnet-4-6, got %q", opts9.NativeModel)
	}

	// 10. CLIProxyAPI thinking suffix that the host passed through: agy/gemini-3.8-flash(low)
	req10 := rpcExecutorRequest{
		Model:   "agy/gemini-3.8-flash(low)",
		Payload: []byte(`{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`),
	}
	opts10 := resolveExecutionOptions(req10, cfg)
	if opts10.ReasoningEffort != "low" || opts10.NativeModel != "gemini-3.8-flash" {
		t.Fatalf("expected low effort on gemini-3.8-flash from (low) suffix, got %q on %q", opts10.ReasoningEffort, opts10.NativeModel)
	}

	// 11. A numeric budget suffix is stripped from the model and falls back to other sources
	req11 := rpcExecutorRequest{
		Model:   "agy/gemini-3.8-flash(8192)",
		Payload: []byte(`{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"medium"}`),
	}
	opts11 := resolveExecutionOptions(req11, cfg)
	if opts11.ReasoningEffort != "medium" || opts11.NativeModel != "gemini-3.8-flash" {
		t.Fatalf("expected payload effort on gemini-3.8-flash, got %q on %q", opts11.ReasoningEffort, opts11.NativeModel)
	}
}

func TestErrorEnvelopeCarriesRetryAfter(t *testing.T) {
	raw := mustErrorEnvelope(&pluginError{Code: "agy_quota_exhausted", Message: "exhausted", HTTPStatus: http.StatusTooManyRequests, Retryable: true, RetryAfterSeconds: 90})
	var env struct {
		OK    bool `json:"ok"`
		Error struct {
			Code              string `json:"code"`
			HTTPStatus        int    `json:"http_status"`
			Retryable         bool   `json:"retryable"`
			RetryAfterSeconds int    `json:"retry_after_seconds"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if env.OK || env.Error.Code != "agy_quota_exhausted" || env.Error.HTTPStatus != 429 || !env.Error.Retryable || env.Error.RetryAfterSeconds != 90 {
		t.Fatalf("unexpected envelope: %s", raw)
	}
}

func TestClassifyAgyErrorModelNotFoundUsesHostCode(t *testing.T) {
	pe := asPluginError(classifyAgyError(errors.New("invalid model selection: nope"), ""))
	if pe.Code != errorCodeModelNotFound || pe.HTTPStatus != http.StatusNotFound {
		t.Fatalf("expected %s/404, got %s/%d", errorCodeModelNotFound, pe.Code, pe.HTTPStatus)
	}
}

func TestParseAgyJSONOutputUsesTerminalLine(t *testing.T) {
	got, err := parseAgyJSONOutput([]byte("diagnostic\n{\"status\":\"SUCCESS\",\"response\":\"ok\"}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "SUCCESS" || got.Response != "ok" {
		t.Fatalf("unexpected result: %#v", got)
	}
}

func TestRouteModel(t *testing.T) {
	invalidateModels()
	cases := []struct {
		model     string
		providers []string
		want      bool
	}{
		{"agy/gemini-3.8-flash", nil, true},
		{"agy/gemini-3.8-flash(high)", nil, true},
		{"agy/default:high", nil, true},
		{"gemini-3.8-flash-high", nil, true},
		{"antigravity/gemini-3.8-flash", nil, true},
		{"antigravity/gemini-3.8-flash", []string{"antigravity"}, false},
		{"gpt-5.5", nil, false},
		{"agy/gemini-3.8-flash", []string{"agy"}, false},
		// A bare "default" is too generic to intercept; only agy/default is ours.
		{"default", nil, false},
		// A configured native provider of the same family keeps the request.
		{"gemini-3.8-flash-high", []string{"gemini"}, false},
		{"claude-sonnet-4-6", []string{"claude"}, false},
		{"claude-sonnet-4-6", []string{"anthropic"}, false},
		{"gpt-oss-120b-medium", []string{"codex"}, false},
		// The explicit agy/ prefix still wins over a native provider.
		{"agy/claude-sonnet-4-6", []string{"claude"}, true},
	}
	for _, tc := range cases {
		resp := routeModel(modelRouteRequest{RequestedModel: tc.model, AvailableProviders: tc.providers})
		if resp.Handled != tc.want || (tc.want && resp.TargetKind != "self") {
			t.Errorf("routeModel(%q, %v) = %+v, want handled=%v", tc.model, tc.providers, resp, tc.want)
		}
	}
}
