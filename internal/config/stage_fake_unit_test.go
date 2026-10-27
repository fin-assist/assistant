package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnitStageFakeModelDropIn(t *testing.T) {
	base, err := os.ReadFile(standUnitFile)
	if err != nil {
		t.Fatal(err)
	}
	drop, err := os.ReadFile(filepath.Join("..", "..", "deploy", "systemd", "assistant.service.d", "fake-llm.conf"))
	if err != nil {
		t.Fatal(err)
	}
	unit, err := parseStandUnit(string(base)+"\n"+string(drop), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkStandSecrets(unit); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := standConfig(unit, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Env != EnvStage || cfg.Auth.Mode != AuthVerify || cfg.Generation.Endpoint != "http://127.0.0.1:8092/v1/responses" || cfg.Generation.ModelURI != "gpt://local/aliceai-llm-flash" || cfg.Generation.APIKeyFile != standCredentials+"/fake-model-key" {
		t.Fatalf("invalid stage fake configuration: env=%s auth=%s endpoint=%s", cfg.Env, cfg.Auth.Mode, cfg.Generation.Endpoint)
	}
	if !strings.Contains(string(drop), "Requires=fake-llm.service") || !strings.Contains(string(drop), "After=fake-llm.service") {
		t.Fatal("stage provider dependency missing")
	}
}

func TestUnitStageFakeProviderUnit(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "systemd", "fake-llm.service"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, directive := range []string{"ExecStart=/opt/finassist/fake-llm/fake-llm -addr 127.0.0.1:8092", "DynamicUser=yes", "NoNewPrivileges=yes", "ProtectSystem=strict", "ProtectHome=yes", "CapabilityBoundingSet=", "TimeoutStopSec=10s"} {
		if !strings.Contains(text, "\n"+directive+"\n") {
			t.Fatal("missing fake provider directive", directive)
		}
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "Environment=") || strings.HasPrefix(line, "LoadCredential=") {
			t.Fatal("synthetic provider needs no external credentials")
		}
	}
}
