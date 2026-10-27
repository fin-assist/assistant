package config

import (
	"errors"
	"math"
	"net"
	"net/url"
	"strings"
)

// GenerationConfig configures model identity and per-question resource limits.
// An empty ModelURI disables generation. Token counts are estimates; request and
// response byte limits are strict. The contract's three-minute deadline is fixed.
type GenerationConfig struct {
	ModelURI                 string
	FolderID                 string
	Endpoint                 string
	APIKeyFile               string
	InputTokensPerStep       int
	InputTokensTotal         int
	HistoryTokens            int
	RequestBytesPerStep      int
	RequestBytesTotal        int
	MaxOutputTokens          int
	MaxSteps                 int
	MaxFunctions             int
	ParallelFunctions        int
	MaxSearchCalls           int
	FinancialResultBytes     int
	FinancialCollectionItems int
	ModelResponseBytes       int
	ModelEventBytes          int
	ModelArgumentBytes       int
	ModelOutputItems         int
}

func (r *reader) generation() GenerationConfig {
	return GenerationConfig{
		ModelURI:                 r.str("ASSISTANT_MODEL_URI"),
		FolderID:                 r.str("ASSISTANT_MODEL_FOLDER_ID"),
		Endpoint:                 r.str("ASSISTANT_MODEL_ENDPOINT"),
		APIKeyFile:               r.str("ASSISTANT_MODEL_API_KEY_FILE"),
		InputTokensPerStep:       r.int("ASSISTANT_INPUT_TOKENS_PER_STEP"),
		InputTokensTotal:         r.int("ASSISTANT_INPUT_TOKENS_TOTAL"),
		HistoryTokens:            r.int("ASSISTANT_HISTORY_TOKENS"),
		RequestBytesPerStep:      r.int("ASSISTANT_REQUEST_BYTES_PER_STEP"),
		RequestBytesTotal:        r.int("ASSISTANT_REQUEST_BYTES_TOTAL"),
		MaxOutputTokens:          r.int("ASSISTANT_MAX_OUTPUT_TOKENS"),
		MaxSteps:                 r.int("ASSISTANT_MAX_STEPS"),
		MaxFunctions:             r.int("ASSISTANT_MAX_FUNCTIONS"),
		ParallelFunctions:        r.int("ASSISTANT_PARALLEL_FUNCTIONS"),
		MaxSearchCalls:           r.int("ASSISTANT_MAX_SEARCH_CALLS"),
		FinancialResultBytes:     r.int("ASSISTANT_FINANCIAL_RESULT_BYTES"),
		FinancialCollectionItems: r.int("ASSISTANT_FINANCIAL_COLLECTION_ITEMS"),
		ModelResponseBytes:       r.int("ASSISTANT_MODEL_RESPONSE_BYTES"),
		ModelEventBytes:          r.int("ASSISTANT_MODEL_EVENT_BYTES"),
		ModelArgumentBytes:       r.int("ASSISTANT_MODEL_ARGUMENT_BYTES"),
		ModelOutputItems:         r.int("ASSISTANT_MODEL_OUTPUT_ITEMS"),
	}
}

func (c Config) validateGeneration() []error {
	g := c.Generation
	var errs []error
	for _, field := range []struct {
		name  string
		value int
	}{
		{"ASSISTANT_INPUT_TOKENS_PER_STEP", g.InputTokensPerStep},
		{"ASSISTANT_INPUT_TOKENS_TOTAL", g.InputTokensTotal},
		{"ASSISTANT_HISTORY_TOKENS", g.HistoryTokens},
		{"ASSISTANT_REQUEST_BYTES_PER_STEP", g.RequestBytesPerStep},
		{"ASSISTANT_REQUEST_BYTES_TOTAL", g.RequestBytesTotal},
		{"ASSISTANT_MAX_OUTPUT_TOKENS", g.MaxOutputTokens},
		{"ASSISTANT_MAX_STEPS", g.MaxSteps},
		{"ASSISTANT_MAX_FUNCTIONS", g.MaxFunctions},
		{"ASSISTANT_PARALLEL_FUNCTIONS", g.ParallelFunctions},
		{"ASSISTANT_MAX_SEARCH_CALLS", g.MaxSearchCalls},
		{"ASSISTANT_FINANCIAL_RESULT_BYTES", g.FinancialResultBytes},
		{"ASSISTANT_FINANCIAL_COLLECTION_ITEMS", g.FinancialCollectionItems},
		{"ASSISTANT_MODEL_RESPONSE_BYTES", g.ModelResponseBytes},
		{"ASSISTANT_MODEL_EVENT_BYTES", g.ModelEventBytes},
		{"ASSISTANT_MODEL_ARGUMENT_BYTES", g.ModelArgumentBytes},
		{"ASSISTANT_MODEL_OUTPUT_ITEMS", g.ModelOutputItems},
	} {
		if field.value <= 0 {
			errs = append(errs, errors.New(field.name+" must be positive"))
		}
	}
	for _, pair := range []struct {
		name        string
		step, total int
	}{
		{"ASSISTANT_INPUT_TOKENS_PER_STEP must not exceed ASSISTANT_INPUT_TOKENS_TOTAL", g.InputTokensPerStep, g.InputTokensTotal},
		{"ASSISTANT_HISTORY_TOKENS must not exceed ASSISTANT_INPUT_TOKENS_PER_STEP", g.HistoryTokens, g.InputTokensPerStep},
		{"ASSISTANT_REQUEST_BYTES_PER_STEP must not exceed ASSISTANT_REQUEST_BYTES_TOTAL", g.RequestBytesPerStep, g.RequestBytesTotal},
		{"ASSISTANT_PARALLEL_FUNCTIONS must not exceed ASSISTANT_MAX_FUNCTIONS", g.ParallelFunctions, g.MaxFunctions},
		{"ASSISTANT_MODEL_EVENT_BYTES must not exceed ASSISTANT_MODEL_RESPONSE_BYTES", g.ModelEventBytes, g.ModelResponseBytes},
		{"ASSISTANT_MODEL_ARGUMENT_BYTES must not exceed ASSISTANT_MODEL_RESPONSE_BYTES", g.ModelArgumentBytes, g.ModelResponseBytes},
	} {
		if pair.step > pair.total {
			errs = append(errs, errors.New(pair.name))
		}
	}
	if g.MaxOutputTokens > 0 && g.MaxOutputTokens < 16 {
		errs = append(errs, errors.New("ASSISTANT_MAX_OUTPUT_TOKENS must be at least 16"))
	}
	if int64(g.ModelResponseBytes) == math.MaxInt64 {
		errs = append(errs, errors.New("ASSISTANT_MODEL_RESPONSE_BYTES is too large"))
	}
	if !validGenerationEndpoint(c.Env, g.Endpoint) {
		errs = append(errs, errors.New("ASSISTANT_MODEL_ENDPOINT must use the official Responses endpoint; dev and stage also allow HTTP loopback (stage requires an IP address)"))
	}
	if g.ModelURI == "" {
		if g.FolderID != "" || g.APIKeyFile != "" {
			errs = append(errs, errors.New("ASSISTANT_MODEL_URI is required with ASSISTANT_MODEL_FOLDER_ID or ASSISTANT_MODEL_API_KEY_FILE"))
		}
		return errs
	}
	if !validGenerationIdentity(g.FolderID, g.ModelURI) {
		errs = append(errs, errors.New("ASSISTANT_MODEL_URI must select Alice LLM or Flash in ASSISTANT_MODEL_FOLDER_ID"))
	}
	if strings.TrimSpace(g.APIKeyFile) == "" || strings.ContainsRune(g.APIKeyFile, 0) {
		errs = append(errs, errors.New("ASSISTANT_MODEL_API_KEY_FILE must name a key file when generation is enabled"))
	}
	return errs
}

func validGenerationIdentity(folder, model string) bool {
	if folder == "" {
		return false
	}
	for _, r := range folder {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return false
		}
	}
	return model == "gpt://"+folder+"/aliceai-llm" || model == "gpt://"+folder+"/aliceai-llm-flash"
}

func validGenerationEndpoint(env Env, endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || u.Path != "/v1/responses" || u.RawPath != "" {
		return false
	}
	if u.Scheme == "https" && u.Host == "ai.api.cloud.yandex.net" {
		return true
	}
	if (env != EnvDev && env != EnvStage) || u.Scheme != "http" {
		return false
	}
	ip := net.ParseIP(u.Hostname())
	return env == EnvDev && u.Hostname() == "localhost" || ip != nil && ip.IsLoopback()
}
