package config

import (
	"math"
	"strconv"
	"strings"
	"testing"
)

func generationEnv() map[string]string {
	vars := base()
	vars["ASSISTANT_MODEL_URI"] = "gpt://folder/aliceai-llm"
	vars["ASSISTANT_MODEL_FOLDER_ID"] = "folder"
	vars["ASSISTANT_MODEL_API_KEY_FILE"] = "/protected/model-key"
	return vars
}

func TestUnitGenerationDefaultsDisabled(t *testing.T) {
	get, list := env(base())
	c, err := load(get, noFiles, list)
	if err != nil {
		t.Fatal(err)
	}
	want := GenerationConfig{Endpoint: "https://ai.api.cloud.yandex.net/v1/responses", InputTokensPerStep: 24000, InputTokensTotal: 48000, HistoryTokens: 16000,
		RequestBytesPerStep: 65536, RequestBytesTotal: 131072, MaxOutputTokens: 4096, MaxSteps: 8, MaxFunctions: 16, ParallelFunctions: 4, MaxSearchCalls: 3,
		FinancialResultBytes: 32768, FinancialCollectionItems: 200, ModelResponseBytes: 1048576, ModelEventBytes: 65536, ModelArgumentBytes: 16384, ModelOutputItems: 64}
	if c.Generation != want {
		t.Fatalf("generation defaults=%+v want=%+v", c.Generation, want)
	}
}

func TestUnitGenerationModelConfiguredWithoutReadingKey(t *testing.T) {
	for _, model := range []string{"aliceai-llm", "aliceai-llm-flash"} {
		for _, environment := range []string{"dev", "stage", "prod"} {
			t.Run(model+environment, func(t *testing.T) {
				vars := withEnv(generationEnv(), environment)
				vars["ASSISTANT_MODEL_URI"] = "gpt://folder/" + model
				get, list := env(vars)
				read := func(string) ([]byte, error) { t.Fatal("configuration opened a model key file"); return nil, nil }
				c, err := load(get, read, list)
				if err != nil {
					t.Fatal(err)
				}
				if c.Generation.ModelURI != vars["ASSISTANT_MODEL_URI"] || c.Generation.FolderID != "folder" || c.Generation.APIKeyFile != "/protected/model-key" {
					t.Fatal("model identity or key path lost")
				}
			})
		}
	}
}

func TestUnitGenerationQuotaOverridesAndBounds(t *testing.T) {
	numeric := []string{"ASSISTANT_INPUT_TOKENS_PER_STEP", "ASSISTANT_INPUT_TOKENS_TOTAL", "ASSISTANT_HISTORY_TOKENS", "ASSISTANT_REQUEST_BYTES_PER_STEP", "ASSISTANT_REQUEST_BYTES_TOTAL", "ASSISTANT_MAX_OUTPUT_TOKENS", "ASSISTANT_MAX_STEPS", "ASSISTANT_MAX_FUNCTIONS", "ASSISTANT_PARALLEL_FUNCTIONS", "ASSISTANT_MAX_SEARCH_CALLS", "ASSISTANT_FINANCIAL_RESULT_BYTES", "ASSISTANT_FINANCIAL_COLLECTION_ITEMS", "ASSISTANT_MODEL_RESPONSE_BYTES", "ASSISTANT_MODEL_EVENT_BYTES", "ASSISTANT_MODEL_ARGUMENT_BYTES", "ASSISTANT_MODEL_OUTPUT_ITEMS"}
	for _, name := range numeric {
		for _, value := range []string{"0", "-1", "invalid", "999999999999999999999999999999"} {
			t.Run(name+value, func(t *testing.T) {
				vars := base()
				vars[name] = value
				get, list := env(vars)
				if _, err := load(get, noFiles, list); err == nil || !strings.Contains(err.Error(), name) {
					t.Fatalf("invalid quota accepted or unnamed: %v", err)
				}
			})
		}
	}
	vars := generationEnv()
	for _, name := range numeric {
		vars[name] = "1"
	}
	vars["ASSISTANT_MAX_OUTPUT_TOKENS"] = "16"
	get, list := env(vars)
	c, err := load(get, noFiles, list)
	if err != nil {
		t.Fatal(err)
	}
	want := GenerationConfig{ModelURI: "gpt://folder/aliceai-llm", FolderID: "folder", Endpoint: "https://ai.api.cloud.yandex.net/v1/responses", APIKeyFile: "/protected/model-key",
		InputTokensPerStep: 1, InputTokensTotal: 1, HistoryTokens: 1, RequestBytesPerStep: 1, RequestBytesTotal: 1, MaxOutputTokens: 16, MaxSteps: 1, MaxFunctions: 1, ParallelFunctions: 1, MaxSearchCalls: 1,
		FinancialResultBytes: 1, FinancialCollectionItems: 1, ModelResponseBytes: 1, ModelEventBytes: 1, ModelArgumentBytes: 1, ModelOutputItems: 1}
	if c.Generation != want {
		t.Fatalf("overrides=%+v", c.Generation)
	}
}

func TestUnitGenerationRelatedLimits(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"ASSISTANT_INPUT_TOKENS_PER_STEP", "48001"},
		{"ASSISTANT_HISTORY_TOKENS", "24001"},
		{"ASSISTANT_REQUEST_BYTES_PER_STEP", "131073"},
		{"ASSISTANT_PARALLEL_FUNCTIONS", "17"},
		{"ASSISTANT_MODEL_EVENT_BYTES", "1048577"},
		{"ASSISTANT_MODEL_ARGUMENT_BYTES", "1048577"},
		{"ASSISTANT_MAX_OUTPUT_TOKENS", "15"},
		{"ASSISTANT_MODEL_RESPONSE_BYTES", strconv.FormatInt(math.MaxInt64, 10)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vars := generationEnv()
			vars[tc.name] = tc.value
			get, list := env(vars)
			if _, err := load(get, noFiles, list); err == nil || !strings.Contains(err.Error(), tc.name) {
				t.Fatalf("inconsistent quota accepted: %v", err)
			}
		})
	}
	vars := generationEnv()
	for name, value := range map[string]string{"ASSISTANT_INPUT_TOKENS_PER_STEP": "48000", "ASSISTANT_HISTORY_TOKENS": "48000", "ASSISTANT_REQUEST_BYTES_PER_STEP": "131072", "ASSISTANT_PARALLEL_FUNCTIONS": "16", "ASSISTANT_MODEL_EVENT_BYTES": "1048576", "ASSISTANT_MODEL_ARGUMENT_BYTES": "1048576"} {
		vars[name] = value
	}
	get, list := env(vars)
	if _, err := load(get, noFiles, list); err != nil {
		t.Fatalf("equal limits rejected: %v", err)
	}
}

func TestUnitGenerationDestinationRestrictions(t *testing.T) {
	for _, tc := range []struct {
		env, endpoint string
		valid         bool
	}{
		{"prod", "https://ai.api.cloud.yandex.net/v1/responses", true},
		{"stage", "https://ai.api.cloud.yandex.net/v1/responses", true},
		{"dev", "http://localhost:8888/v1/responses", true},
		{"dev", "http://127.0.0.1:8888/v1/responses", true},
		{"dev", "http://[::1]:8888/v1/responses", true},
		{"prod", "http://127.0.0.1:8888/v1/responses", false},
		{"stage", "http://localhost:8888/v1/responses", false},
		{"dev", "https://localhost/v1/responses", false},
		{"dev", "http://192.0.2.1/v1/responses", false},
		{"prod", "https://ai.api.cloud.yandex.net.evil/v1/responses", false},
		{"prod", "https://ai.api.cloud.yandex.net:443/v1/responses", false},
		{"prod", "http://ai.api.cloud.yandex.net/v1/responses", false},
		{"prod", "https://user:private-value@ai.api.cloud.yandex.net/v1/responses", false},
		{"prod", "https://ai.api.cloud.yandex.net/v1/responses?key=private-value", false},
		{"prod", "https://ai.api.cloud.yandex.net/v1/responses?", false},
		{"prod", "https://ai.api.cloud.yandex.net/v1/responses#private-value", false},
		{"prod", "https://ai.api.cloud.yandex.net/v1/%72esponses", false},
		{"dev", "http://localhost/v1/responses/", false},
		{"dev", "http://localhost/other", false},
		{"dev", "%private-value", false},
	} {
		t.Run(tc.env+tc.endpoint, func(t *testing.T) {
			vars := withEnv(generationEnv(), tc.env)
			vars["ASSISTANT_MODEL_ENDPOINT"] = tc.endpoint
			get, list := env(vars)
			_, err := load(get, noFiles, list)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
			if err != nil && strings.Contains(err.Error(), "private-value") {
				t.Fatal("endpoint leaked into error")
			}
		})
	}
}

func TestUnitGenerationIdentityAndSecretPathErrors(t *testing.T) {
	for _, values := range []map[string]string{
		{"ASSISTANT_MODEL_FOLDER_ID": "other-private-value"},
		{"ASSISTANT_MODEL_FOLDER_ID": "private/value"},
		{"ASSISTANT_MODEL_FOLDER_ID": " folder"},
		{"ASSISTANT_MODEL_URI": "gpt://folder/aliceai-llm/private-value"},
		{"ASSISTANT_MODEL_URI": "https://private-value"},
		{"ASSISTANT_MODEL_URI": " "},
		{"ASSISTANT_MODEL_API_KEY_FILE": " \t\n"},
		{"ASSISTANT_MODEL_API_KEY_FILE": "/private-value\x00key"},
	} {
		vars := generationEnv()
		for name, value := range values {
			vars[name] = value
		}
		get, list := env(vars)
		_, err := load(get, noFiles, list)
		if err == nil || strings.Contains(err.Error(), "private-value") || strings.Contains(err.Error(), "/private") {
			t.Fatalf("invalid identity accepted or exposed: %v", err)
		}
	}
}

func TestUnitGenerationRejectsPartialConfiguration(t *testing.T) {
	for _, values := range []map[string]string{
		{"ASSISTANT_MODEL_FOLDER_ID": "folder"},
		{"ASSISTANT_MODEL_API_KEY_FILE": "/protected/key"},
		{"ASSISTANT_MODEL_FOLDER_ID": "folder", "ASSISTANT_MODEL_API_KEY_FILE": "/protected/key"},
		{"ASSISTANT_MODEL_URI": "gpt://folder/aliceai-llm"},
		{"ASSISTANT_MODEL_URI": "gpt://folder/aliceai-llm", "ASSISTANT_MODEL_FOLDER_ID": "folder"},
		{"ASSISTANT_MODEL_URI": "gpt://folder/aliceai-llm", "ASSISTANT_MODEL_API_KEY_FILE": "/protected/key"},
	} {
		vars := base()
		for k, v := range values {
			vars[k] = v
		}
		get, list := env(vars)
		if _, err := load(get, noFiles, list); err == nil {
			t.Fatalf("partial generation accepted: %v", values)
		}
	}
}
