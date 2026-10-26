package config

import (
	"bufio"
	"errors"
	"fmt"
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

// standModelEnv is what an operator adds to the env file with the optional model block.
var standModelEnv = map[string]string{"ASSISTANT_MODEL_URI": "gpt://b1g-folder/aliceai-llm", "ASSISTANT_MODEL_FOLDER_ID": "b1g-folder"}

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

// parseStandUnit reads the active directives of the unit text; with optional it also reads
// the commented-out ("#Key=") directives of the optional blocks as if an operator
// uncommented them. Directive keys are matched as systemd does, with spaces around "="
// allowed; the directives the test reads must be in [Service].
func parseStandUnit(text string, optional bool) (standUnit, error) {
	unit := standUnit{env: map[string]string{}, credentials: map[string]string{}}
	var comments strings.Builder
	section := ""
	scanner := bufio.NewScanner(strings.NewReader(text))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = line
			continue
		}
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
		case "SetCredential", "SetCredentialEncrypted", "LoadCredentialEncrypted", "ImportCredential":
			// The secret would be written into the unit itself or come from a source the
			// checks below do not cover; these share the credential table with LoadCredential=.
			return unit, fmt.Errorf("unit line %q: secrets come from files by LoadCredential=, not %s=", line, key)
		default:
			continue
		}
		if section != "[Service]" {
			return unit, fmt.Errorf("unit line %q is in %q, want [Service]", line, section)
		}
		if commented && !optional {
			continue
		}
		switch key {
		case "Environment":
			// Only the plain form NAME=value: systemd would unquote or split anything else.
			name, setting, ok := strings.Cut(value, "=")
			if !ok || name == "" || strings.ContainsAny(value, "\"' \t\\") {
				return unit, fmt.Errorf("unit line %q: use the plain form Environment=NAME=value without quotes, spaces or backslashes", line)
			}
			if _, dup := unit.env[name]; dup {
				return unit, fmt.Errorf("unit sets %s twice", name)
			}
			unit.env[name] = strings.ReplaceAll(setting, "%d", standCredentials)
		case "LoadCredential":
			name, source, ok := strings.Cut(value, ":")
			if !ok || name == "" {
				return unit, fmt.Errorf("unit line %q: want LoadCredential=<name>:<source>", line)
			}
			// Which duplicate systemd keeps depends on its version, so a repeat is rejected.
			if _, dup := unit.credentials[name]; dup {
				return unit, fmt.Errorf("unit loads credential %s twice", name)
			}
			unit.credentials[name] = source
		case "EnvironmentFile":
			unit.envFiles = append(unit.envFiles, value)
		}
	}
	if err := scanner.Err(); err != nil {
		return unit, err
	}
	unit.comments = comments.String()
	return unit, nil
}

// credentialContent is what the fake credentials directory holds for a credential: a
// valid DSN that names the credential, so a value read from the wrong one is visible.
func credentialContent(name string) string { return "postgres://stand.invalid/" + name }

// checkStandSecrets allows only variables the service reads, requires every secret to
// come from a loaded credential, every credential to come from its own file under the
// service directory and to be read by some variable of the unit.
func checkStandSecrets(unit standUnit) error {
	secretFiles := append([]string(nil), standFileSecrets...)
	allowed := map[string]bool{}
	for _, v := range Vars {
		if !v.Secret {
			allowed[v.Name] = true
			continue
		}
		if _, ok := unit.env[v.Name]; ok {
			return fmt.Errorf("unit sets secret %s as a value; use %s_FILE with a credential", v.Name, v.Name)
		}
		secretFiles = append(secretFiles, v.Name+"_FILE")
	}
	for _, name := range secretFiles {
		allowed[name] = true
	}
	// Anything else (PGPASSWORD=...) would be a value in the unit that no check covers.
	for name := range unit.env {
		if !allowed[name] && !peerVar.MatchString(name) {
			return fmt.Errorf("unit sets %s, which is not a non-secret variable of Vars, a <SECRET>_FILE or PEER_<NAME>_ADDR", name)
		}
	}
	for _, name := range secretFiles {
		value, ok := unit.env[name]
		if !ok {
			continue
		}
		credential, inDir := strings.CutPrefix(value, standCredentials+"/")
		if !inDir || strings.Contains(credential, "/") || unit.credentials[credential] == "" {
			return fmt.Errorf("%s=%q: want %s/<name> of a LoadCredential= of the unit", name, value, standCredentials)
		}
	}
	read := map[string]bool{}
	for name, value := range unit.env {
		if credential, ok := strings.CutPrefix(value, standCredentials+"/"); ok {
			if unit.credentials[credential] == "" {
				return fmt.Errorf("%s names credential %q that the unit does not load", name, credential)
			}
			read[credential] = true
		}
	}
	for name, source := range unit.credentials {
		clean := filepath.Clean(source)
		base := filepath.Base(clean)
		if filepath.Dir(clean) != standSources || base != name && strings.TrimSuffix(base, filepath.Ext(base)) != name {
			return fmt.Errorf("credential %s comes from %q, want %s/%s[.<ext>]", name, source, standSources, name)
		}
		if !read[name] {
			return fmt.Errorf("credential %s is loaded but no variable of the unit reads it", name)
		}
	}
	return nil
}

// standConfig loads the configuration the unit gives the process with envFile as the
// content of its only EnvironmentFile=. The fake file system holds only the credentials
// the unit loads, so a secret read from any other path fails the load.
func standConfig(unit standUnit, envFile map[string]string) (Config, func(string) ([]byte, error), error) {
	if err := checkStandSecrets(unit); err != nil {
		return Config{}, nil, err
	}
	if len(unit.envFiles) != 1 || unit.envFiles[0] != standEnvFile {
		return Config{}, nil, fmt.Errorf("unit EnvironmentFile= %q, want exactly %s", unit.envFiles, standEnvFile)
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
			return Config{}, nil, fmt.Errorf("the env file would override the unit setting %s", name)
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
		return Config{}, nil, fmt.Errorf("stand unit configuration rejected: %w", err)
	}
	if c.DB.PrimaryDSN != credentialContent("db-primary-dsn") {
		return Config{}, nil, fmt.Errorf("primary DSN %q, want the content of credential db-primary-dsn", c.DB.PrimaryDSN)
	}
	return c, readFile, nil
}

// checkStandActive checks the unit as installed. FIN-347: it sets ENV=stage and keeps the
// default AUTH_MODE=verify, so it must itself pass the stand key files; with only the
// non-secret settings its comment names in the env file, the configuration loads.
// FIN-349: the model block is optional, so the active unit runs without generation.
func checkStandActive(text string) error {
	unit, err := parseStandUnit(text, false)
	if err != nil {
		return err
	}
	if unit.env["ENV"] != "stage" {
		return fmt.Errorf("unit ENV=%q, want stage", unit.env["ENV"])
	}
	// SERVICE_NAME is the kid/sub of the stand signing key, fixed by the unit itself.
	if unit.env["SERVICE_NAME"] != standServiceName {
		return fmt.Errorf("unit SERVICE_NAME=%q, want %s", unit.env["SERVICE_NAME"], standServiceName)
	}
	for _, name := range []string{"AUTH_STAGE_SIGNING_KEY_FILE", "AUTH_STAGE_PUBLIC_KEYS_FILE", "DB_PRIMARY_DSN_FILE"} {
		if unit.env[name] == "" {
			return fmt.Errorf("unit does not set %s", name)
		}
	}
	c, _, err := standConfig(unit, nil)
	if err != nil {
		return err
	}
	if c.Env != EnvStage || c.Auth.Mode != AuthVerify {
		return fmt.Errorf("stand runs ENV=%s AUTH_MODE=%s, want stage and verify", c.Env, c.Auth.Mode)
	}
	if c.Auth.Identity.StageSigningKeyFile != standCredentials+"/stage-signing" || c.Auth.Identity.StagePublicKeysFile == "" {
		return fmt.Errorf("stage key files: %+v", c.Auth.Identity)
	}
	if c.Generation.ModelURI != "" || c.Generation.APIKeyFile != "" {
		return fmt.Errorf("the active unit enables generation: %+v", c.Generation)
	}
	return nil
}

// checkStandModelBlock checks the unit with the optional model block uncommented (FIN-349):
// the model key is the loaded credential model-api-key, and with the model settings in the
// env file the configuration enables generation. The key file is read by the transport,
// not by the configuration. Generation also needs the bank-statement-data and analytics
// peers (internal/app/wire.go), which the configuration does not check, so the comment of
// the block must name them.
func checkStandModelBlock(text string) error {
	unit, err := parseStandUnit(text, true)
	if err != nil {
		return err
	}
	for _, name := range []string{"ASSISTANT_MODEL_URI", "ASSISTANT_MODEL_FOLDER_ID", "PEER_BANK_STATEMENT_DATA_ADDR", "PEER_ANALYTICS_ADDR"} {
		if !strings.Contains(unit.comments, name) {
			return fmt.Errorf("the unit comment does not name %s for the model block", name)
		}
	}
	c, readFile, err := standConfig(unit, standModelEnv)
	if err != nil {
		return err
	}
	if c.Generation.ModelURI != standModelEnv["ASSISTANT_MODEL_URI"] {
		return fmt.Errorf("generation: %+v", c.Generation)
	}
	key, err := readFile(c.Generation.APIKeyFile)
	if err != nil || string(key) != credentialContent("model-api-key") {
		return fmt.Errorf("model key file %q holds %q (%w), want the content of credential model-api-key", c.Generation.APIKeyFile, key, err)
	}
	return nil
}

func checkStandUnit(text string) error {
	return errors.Join(checkStandActive(text), checkStandModelBlock(text))
}

func readStandUnitText(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(standUnitFile)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestUnitStandUnitSatisfiesStageIdentity(t *testing.T) {
	if err := checkStandActive(readStandUnitText(t)); err != nil {
		t.Fatal(err)
	}
}

func TestUnitStandUnitModelBlockEnablesGeneration(t *testing.T) {
	if err := checkStandModelBlock(readStandUnitText(t)); err != nil {
		t.Fatal(err)
	}
}

// The checks above reject each way the unit can drift from the configuration and accept
// the legitimate variations: every case edits the real unit text.
func TestUnitStandUnitCheckRejectsMutations(t *testing.T) {
	const (
		dsn       = "LoadCredential=db-primary-dsn:/etc/finassist/assistant/db-primary-dsn\n"
		dsnEnv    = "Environment=DB_PRIMARY_DSN_FILE=%d/db-primary-dsn\n"
		signEnv   = "Environment=AUTH_STAGE_SIGNING_KEY_FILE=%d/stage-signing\n"
		modelKey  = "#LoadCredential=model-api-key:/etc/finassist/assistant/model-api-key\n"
		modelEnv  = "#Environment=ASSISTANT_MODEL_API_KEY_FILE=%d/model-api-key\n"
		envFile   = "EnvironmentFile=/etc/finassist/assistant/env\n"
		envStage  = "Environment=ENV=stage\n"
		peerNames = "PEER_BANK_STATEMENT_DATA_ADDR and PEER_ANALYTICS_ADDR"
	)
	type edit struct{ old, new string }
	cases := []struct {
		name  string
		edits []edit
		want  string // substring of the error; empty: the unit is accepted
	}{
		{"direct DSN path without credential", []edit{{dsn, ""}, {dsnEnv, "Environment=DB_PRIMARY_DSN_FILE=/etc/finassist/assistant/db-primary-dsn\n"}}, "DB_PRIMARY_DSN_FILE="},
		{"secret as value", []edit{{dsnEnv, dsnEnv + "Environment=DB_REPLICA_DSN=postgres://r/x\n"}}, "unit sets secret DB_REPLICA_DSN"},
		{"undeclared variable", []edit{{dsnEnv, dsnEnv + "Environment=PGPASSWORD=secret\n"}}, "unit sets PGPASSWORD"},
		{"SetCredential", []edit{{dsn, dsn + "SetCredential=db-primary-dsn:postgres://x\n"}}, "not SetCredential="},
		{"directive outside [Service]", []edit{{"[Install]\n", "[Install]\n" + envStage}}, `in "[Install]"`},
		{"source outside the service directory", []edit{{dsn, "LoadCredential=db-primary-dsn:/etc/finassist/other/db-primary-dsn\n"}}, "credential db-primary-dsn comes from"},
		{"source of another name", []edit{{dsn, "LoadCredential=db-primary-dsn:/etc/finassist/assistant/missing-dsn\n"}}, "credential db-primary-dsn comes from"},
		{"source through ..", []edit{{dsn, "LoadCredential=db-primary-dsn:/etc/finassist/assistant/../other/db-primary-dsn\n"}}, "credential db-primary-dsn comes from"},
		{"optional source outside the service directory", []edit{{modelKey, "#LoadCredential=model-api-key:/etc/finassist/x/model-api-key\n"}}, "credential model-api-key comes from"},
		{"two env files", []edit{{envFile, envFile + "EnvironmentFile=/etc/finassist/assistant/env2\n"}}, "want exactly"},
		{"env file of another service", []edit{{envFile, "EnvironmentFile=/etc/finassist/other/env\n"}}, "want exactly"},
		{"quoted Environment", []edit{{envStage, `Environment="ENV=stage"` + "\n"}}, "plain form"},
		{"space in Environment", []edit{{envStage, "Environment=ENV=stage SERVICE_NAME=x\n"}}, "plain form"},
		{"backslash in Environment", []edit{{envStage, `Environment=ENV=sta\ge` + "\n"}}, "plain form"},
		{"tab in Environment", []edit{{envStage, "Environment=ENV=stage\tx\n"}}, "plain form"},
		{"quoted optional Environment", []edit{{modelEnv, `#Environment=ASSISTANT_MODEL_API_KEY_FILE="%d/model-api-key"` + "\n"}}, "plain form"},
		{"spaces around = in LoadCredential", []edit{{dsn, "LoadCredential = db-primary-dsn:/etc/finassist/other/db-primary-dsn\n"}}, "credential db-primary-dsn comes from"},
		{"spaces around = in Environment", []edit{{dsnEnv, dsnEnv + "Environment = DB_REPLICA_DSN=postgres://r/x\n"}}, "unit sets secret DB_REPLICA_DSN"},
		{"spaces around = in optional LoadCredential", []edit{{modelKey, "#LoadCredential = model-api-key:/etc/finassist/x/model-api-key\n"}}, "credential model-api-key comes from"},
		{"unread credential", []edit{{dsn, dsn + "LoadCredential=jwks:/etc/finassist/assistant/jwks\n"}}, "credential jwks is loaded"},
		{"unread optional credential", []edit{{modelKey, modelKey + "#LoadCredential=jwks:/etc/finassist/assistant/jwks\n"}}, "credential jwks is loaded"},
		{"model key from another credential", []edit{{modelKey, ""}, {modelEnv, "#Environment=ASSISTANT_MODEL_API_KEY_FILE=%d/stage-signing\n"}}, "want the content of credential model-api-key"},
		{"DSN from another credential", []edit{{dsn, ""}, {dsnEnv, "Environment=DB_PRIMARY_DSN_FILE=%d/stage-signing\n"}}, "want the content of credential db-primary-dsn"},
		{"model comment without a peer", []edit{{peerNames, "PEER_BANK_STATEMENT_DATA_ADDR"}}, "does not name PEER_ANALYTICS_ADDR"},
		{"signing key outside credentials", []edit{{signEnv, "Environment=AUTH_STAGE_SIGNING_KEY_FILE=/etc/finassist/assistant/stage-signing.pem\n"}}, "AUTH_STAGE_SIGNING_KEY_FILE="},
		{"model block uncommented in the installed unit", []edit{{modelKey, modelKey[1:]}, {modelEnv, modelEnv[1:]}}, "ASSISTANT_MODEL_URI is required with"},
		{"model credential line left as prose", []edit{{modelKey, "# " + modelKey[1:]}}, "ASSISTANT_MODEL_API_KEY_FILE="},
		{"model key line left as prose", []edit{{modelEnv, "# " + modelEnv[1:]}}, "credential model-api-key is loaded"},
		{"model block removed", []edit{{modelKey, ""}, {modelEnv, ""}}, "ASSISTANT_MODEL_API_KEY_FILE must name a key file"},
		{"credential loaded twice", []edit{{dsn, "LoadCredential=db-primary-dsn:/etc/finassist/other/db-primary-dsn\n" + dsn}}, "unit loads credential db-primary-dsn twice"},
		{"LoadCredentialEncrypted", []edit{{dsn, dsn + "LoadCredentialEncrypted=tls.key:/etc/finassist/assistant/tls.key\n"}}, "not LoadCredentialEncrypted="},
		{"ImportCredential", []edit{{dsn, dsn + "ImportCredential=finassist.*\n"}}, "not ImportCredential="},
		{"accepted: spaces around =", []edit{{dsn, "LoadCredential = db-primary-dsn:/etc/finassist/assistant/db-primary-dsn\n"}}, ""},
		{"accepted: credential name with a dot", []edit{{dsn, dsn + "LoadCredential=tls.key:/etc/finassist/assistant/tls.key\nEnvironment=SERVICE_TOKEN_FILE=%d/tls.key\n"}}, ""},
		{"accepted: replica credential", []edit{{dsn, dsn + "LoadCredential=db-replica-dsn:/etc/finassist/assistant/db-replica-dsn\nEnvironment=DB_REPLICA_DSN_FILE=%d/db-replica-dsn\n"}}, ""},
	}
	original := readStandUnitText(t)
	if err := checkStandUnit(original); err != nil {
		t.Fatalf("the unit itself: %v", err)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text := original
			for _, e := range tc.edits {
				if strings.Count(text, e.old) != 1 {
					t.Fatalf("the unit does not hold %q exactly once", e.old)
				}
				text = strings.Replace(text, e.old, e.new, 1)
			}
			err := checkStandUnit(text)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("legitimate unit rejected: %v", err)
			case tc.want != "" && err == nil:
				t.Fatalf("unit accepted, want an error with %q", tc.want)
			case tc.want != "" && !strings.Contains(err.Error(), tc.want):
				t.Fatalf("error %v, want %q", err, tc.want)
			}
		})
	}
}
