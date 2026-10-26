package config

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// standUnit is the settings the example systemd unit of the stand gives the process:
// active Environment= lines with %d resolved to the credentials directory, and the
// LoadCredential= names that directory holds.
type standUnit struct {
	env         map[string]string
	credentials map[string]bool
}

const standCredentials = "/run/credentials/assistant.service"

func readStandUnit(t *testing.T) standUnit {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "deploy", "systemd", "assistant.service"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	unit := standUnit{env: map[string]string{}, credentials: map[string]bool{}}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if value, ok := strings.CutPrefix(line, "Environment="); ok {
			name, setting, found := strings.Cut(value, "=")
			if !found {
				t.Fatalf("malformed unit line %q", line)
			}
			unit.env[name] = strings.ReplaceAll(setting, "%d", standCredentials)
		}
		if value, ok := strings.CutPrefix(line, "LoadCredential="); ok {
			name, _, _ := strings.Cut(value, ":")
			unit.credentials[name] = true
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return unit
}

// FIN-347: the example unit sets ENV=stage and keeps the default AUTH_MODE=verify, so it
// must itself pass the stand key files; with only the non-secret settings its comment names
// in the env file, the configuration of the stand loads.
func TestUnitStandUnitSatisfiesStageIdentity(t *testing.T) {
	unit := readStandUnit(t)
	if unit.env["ENV"] != "stage" {
		t.Fatalf("unit ENV=%q, want stage", unit.env["ENV"])
	}
	// SERVICE_NAME is the kid/sub of the stand signing key, fixed by the unit itself.
	if unit.env["SERVICE_NAME"] != "assistant" {
		t.Fatalf("unit SERVICE_NAME=%q, want assistant", unit.env["SERVICE_NAME"])
	}
	for _, name := range []string{"AUTH_STAGE_SIGNING_KEY_FILE", "AUTH_STAGE_PUBLIC_KEYS_FILE", "DB_PRIMARY_DSN_FILE"} {
		if unit.env[name] == "" {
			t.Fatalf("unit does not set %s", name)
		}
	}
	for name, value := range unit.env {
		if credential, ok := strings.CutPrefix(value, standCredentials+"/"); ok && !unit.credentials[credential] {
			t.Fatalf("%s names credential %q that the unit does not load", name, credential)
		}
	}
	vars := map[string]string{
		// The env file of the stand: only non-secret settings named in the unit comment.
		"AUTH_STAGE_ISSUER": "stage-services", "AUTH_USER_ISSUER": "https://user.example", "AUTH_USER_AUDIENCE": "finassist", "PEER_USER_ADDR": "dns:///user:9090",
	}
	// systemd applies EnvironmentFile= over Environment=: an env file that repeated a
	// unit setting would silently replace it.
	for name, value := range unit.env {
		if _, ok := vars[name]; ok {
			t.Fatalf("the env file would override the unit setting %s", name)
		}
		vars[name] = value
	}
	dsnFile := unit.env["DB_PRIMARY_DSN_FILE"]
	readFile := func(path string) ([]byte, error) {
		if path == dsnFile {
			return []byte("postgres://primary/assistant"), nil
		}
		return noFiles(path)
	}
	get, list := env(vars)
	c, err := load(get, readFile, list)
	if err != nil {
		t.Fatalf("stand unit configuration rejected: %v", err)
	}
	if c.Env != EnvStage || c.Auth.Mode != AuthVerify {
		t.Fatalf("stand runs ENV=%s AUTH_MODE=%s, want stage and verify", c.Env, c.Auth.Mode)
	}
	if c.Auth.Identity.StageSigningKeyFile != standCredentials+"/stage-signing" || c.Auth.Identity.StagePublicKeysFile == "" {
		t.Fatalf("stage key files: %+v", c.Auth.Identity)
	}
}
