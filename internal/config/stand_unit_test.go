package config

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// standServiceName is the service the example systemd unit of the stand runs; the unit
// file, the credentials directory, the env file and the credential sources follow from it.
const (
	standServiceName = "assistant"
	standCredentials = "/run/credentials/" + standServiceName + ".service"
	standEnvFile     = "/etc/finassist/" + standServiceName + "/env"
	standSources     = "/etc/finassist/" + standServiceName
)

var standUnitFile = filepath.Join("..", "..", "deploy", "systemd", standServiceName+".service")

// standFileSecrets are file-only secrets: variables that name a key file and have no
// value form, so a declared Secret of Vars does not cover them.
var standFileSecrets = []string{"AUTH_STAGE_SIGNING_KEY_FILE", "ASSISTANT_MODEL_API_KEY_FILE", "SERVICE_TOKEN_FILE"}

// standUnit is the settings the example systemd unit of the stand gives the process:
// Environment= lines with %d resolved to the credentials directory, the LoadCredential=
// names that directory holds with their sources, the EnvironmentFile= paths and the
// comment text of the unit.
type standUnit struct {
	env         map[string]string
	credentials map[string]string // name -> source
	envFiles    []string
	comments    string
}

// readStandUnit reads the active directives; with optional it also reads the
// commented-out ("#Key=") directives of the optional blocks as if an operator uncommented them.
// Directive keys are matched as systemd does, with spaces around "=" allowed.
func readStandUnit(t *testing.T, optional bool) standUnit {
	t.Helper()
	f, err := os.Open(standUnitFile)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	unit := standUnit{env: map[string]string{}, credentials: map[string]string{}}
	var comments strings.Builder
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		commented := false
		if rest, ok := strings.CutPrefix(line, "#"); ok {
			comments.WriteString(rest + "\n")
			// A directive of an optional block is commented out with "#" directly before
			// its key; "# ..." is prose.
			if rest == "" || rest[0] == ' ' || rest[0] == '\t' {
				continue
			}
			line, commented = rest, true
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		switch key {
		case "Environment", "LoadCredential", "EnvironmentFile":
		default:
			continue
		}
		if commented && !optional {
			continue
		}
		switch key {
		case "Environment":
			// Only the plain form NAME=value: systemd would unquote or split anything else.
			name, setting, ok := strings.Cut(value, "=")
			if !ok || name == "" || strings.ContainsAny(value, "\"' \t\\") {
				t.Fatalf("unit line %q: use the plain form Environment=NAME=value without quotes, spaces or backslashes", line)
			}
			if _, dup := unit.env[name]; dup {
				t.Fatalf("unit sets %s twice", name)
			}
			unit.env[name] = strings.ReplaceAll(setting, "%d", standCredentials)
		case "LoadCredential":
			name, source, ok := strings.Cut(value, ":")
			if !ok || name == "" {
				t.Fatalf("unit line %q: want LoadCredential=<name>:<source>", line)
			}
			unit.credentials[name] = source
		case "EnvironmentFile":
			unit.envFiles = append(unit.envFiles, value)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	unit.comments = comments.String()
	return unit
}

// credentialContent is what the fake credentials directory holds for a credential: a
// valid DSN that names the credential, so a value read from the wrong one is visible.
func credentialContent(name string) string { return "postgres://stand.invalid/" + name }

// checkStandSecrets requires every secret of the unit to come from a loaded credential,
// every credential to come from its own file under the service directory and to be read
// by some variable of the unit.
func checkStandSecrets(t *testing.T, unit standUnit) {
	t.Helper()
	secretFiles := append([]string(nil), standFileSecrets...)
	for _, v := range Vars {
		if !v.Secret {
			continue
		}
		if _, ok := unit.env[v.Name]; ok {
			t.Fatalf("unit sets secret %s as a value; use %s_FILE with a credential", v.Name, v.Name)
		}
		secretFiles = append(secretFiles, v.Name+"_FILE")
	}
	for _, name := range secretFiles {
		value, ok := unit.env[name]
		if !ok {
			continue
		}
		credential, inDir := strings.CutPrefix(value, standCredentials+"/")
		if !inDir || strings.Contains(credential, "/") || unit.credentials[credential] == "" {
			t.Fatalf("%s=%q: want %s/<name> of a LoadCredential= of the unit", name, value, standCredentials)
		}
	}
	read := map[string]bool{}
	for name, value := range unit.env {
		if credential, ok := strings.CutPrefix(value, standCredentials+"/"); ok {
			if unit.credentials[credential] == "" {
				t.Fatalf("%s names credential %q that the unit does not load", name, credential)
			}
			read[credential] = true
		}
	}
	for name, source := range unit.credentials {
		clean := filepath.Clean(source)
		base := filepath.Base(clean)
		if filepath.Dir(clean) != standSources || strings.TrimSuffix(base, filepath.Ext(base)) != name {
			t.Fatalf("credential %s comes from %q, want %s/%s[.<ext>]", name, source, standSources, name)
		}
		if !read[name] {
			t.Fatalf("credential %s is loaded but no variable of the unit reads it", name)
		}
	}
}

// standConfig loads the configuration the unit gives the process with envFile as the
// content of its only EnvironmentFile=. The fake file system holds only the credentials
// the unit loads, so a secret read from any other path fails the load.
func standConfig(t *testing.T, unit standUnit, envFile map[string]string) (Config, func(string) ([]byte, error)) {
	t.Helper()
	checkStandSecrets(t, unit)
	if len(unit.envFiles) != 1 || unit.envFiles[0] != standEnvFile {
		t.Fatalf("unit EnvironmentFile= %q, want exactly %s", unit.envFiles, standEnvFile)
	}
	// The env file of the stand: only non-secret settings named in the unit comment.
	file := map[string]string{
		"AUTH_STAGE_ISSUER": "stage-services", "AUTH_USER_ISSUER": "https://user.example", "AUTH_USER_AUDIENCE": "finassist", "PEER_USER_ADDR": "dns:///user:9090",
	}
	for name, value := range envFile {
		file[name] = value
	}
	vars := map[string]string{}
	for name, value := range unit.env {
		vars[name] = value
	}
	// systemd applies EnvironmentFile= over Environment=: an env file that repeated a
	// unit setting would silently replace it.
	for name, value := range file {
		if _, ok := unit.env[name]; ok {
			t.Fatalf("the env file would override the unit setting %s", name)
		}
		vars[name] = value
	}
	readFile := func(p string) ([]byte, error) {
		if credential, ok := strings.CutPrefix(p, standCredentials+"/"); ok && unit.credentials[credential] != "" {
			return []byte(credentialContent(credential)), nil
		}
		return noFiles(p)
	}
	get, list := env(vars)
	c, err := load(get, readFile, list)
	if err != nil {
		t.Fatalf("stand unit configuration rejected: %v", err)
	}
	return c, readFile
}

// FIN-347: the example unit sets ENV=stage and keeps the default AUTH_MODE=verify, so it
// must itself pass the stand key files; with only the non-secret settings its comment names
// in the env file, the configuration of the stand loads. FIN-349: the model block is
// optional, so the active unit runs without generation.
func TestUnitStandUnitSatisfiesStageIdentity(t *testing.T) {
	unit := readStandUnit(t, false)
	if unit.env["ENV"] != "stage" {
		t.Fatalf("unit ENV=%q, want stage", unit.env["ENV"])
	}
	// SERVICE_NAME is the kid/sub of the stand signing key, fixed by the unit itself.
	if unit.env["SERVICE_NAME"] != standServiceName {
		t.Fatalf("unit SERVICE_NAME=%q, want %s", unit.env["SERVICE_NAME"], standServiceName)
	}
	for _, name := range []string{"AUTH_STAGE_SIGNING_KEY_FILE", "AUTH_STAGE_PUBLIC_KEYS_FILE", "DB_PRIMARY_DSN_FILE"} {
		if unit.env[name] == "" {
			t.Fatalf("unit does not set %s", name)
		}
	}
	c, _ := standConfig(t, unit, nil)
	if c.Env != EnvStage || c.Auth.Mode != AuthVerify {
		t.Fatalf("stand runs ENV=%s AUTH_MODE=%s, want stage and verify", c.Env, c.Auth.Mode)
	}
	if c.DB.PrimaryDSN != credentialContent("db-primary-dsn") {
		t.Fatalf("primary DSN %q, want the content of credential db-primary-dsn", c.DB.PrimaryDSN)
	}
	if c.Auth.Identity.StageSigningKeyFile != standCredentials+"/stage-signing" || c.Auth.Identity.StagePublicKeysFile == "" {
		t.Fatalf("stage key files: %+v", c.Auth.Identity)
	}
	if c.Generation.ModelURI != "" || c.Generation.APIKeyFile != "" {
		t.Fatalf("the active unit enables generation: %+v", c.Generation)
	}
}

// FIN-349: uncommenting the optional model block gives the model key as the loaded
// credential model-api-key, and with the model settings in the env file the configuration
// enables generation. The key file is read by the transport, not by the configuration.
// Generation also needs the bank-statement-data and analytics peers (internal/app/wire.go),
// which the configuration does not check, so the comment of the block must name them.
func TestUnitStandUnitModelBlockEnablesGeneration(t *testing.T) {
	unit := readStandUnit(t, true)
	for _, name := range []string{"ASSISTANT_MODEL_URI", "ASSISTANT_MODEL_FOLDER_ID", "PEER_BANK_STATEMENT_DATA_ADDR", "PEER_ANALYTICS_ADDR"} {
		if !strings.Contains(unit.comments, name) {
			t.Fatalf("the unit comment does not name %s for the model block", name)
		}
	}
	c, readFile := standConfig(t, unit, map[string]string{
		"ASSISTANT_MODEL_URI": "gpt://b1g-folder/aliceai-llm", "ASSISTANT_MODEL_FOLDER_ID": "b1g-folder",
	})
	if c.Generation.ModelURI != "gpt://b1g-folder/aliceai-llm" {
		t.Fatalf("generation: %+v", c.Generation)
	}
	key, err := readFile(c.Generation.APIKeyFile)
	if err != nil || string(key) != credentialContent("model-api-key") {
		t.Fatalf("model key file %q holds %q (%v), want the content of credential model-api-key", c.Generation.APIKeyFile, key, err)
	}
	if c.DB.PrimaryDSN != credentialContent("db-primary-dsn") {
		t.Fatalf("primary DSN %q, want the content of credential db-primary-dsn", c.DB.PrimaryDSN)
	}
}
