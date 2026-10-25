package config

import (
	"strings"
	"testing"
)

func validIdentityConfig() Config {
	return Config{Env: EnvProd, Auth: AuthConfig{Mode: AuthVerify, ServiceTokenFile: "/internal-token", Identity: IdentityConfig{
		UserIssuer: "https://user.example", UserAudience: "finassist", KubernetesIssuer: "https://cluster.example", KubernetesJWKSURL: "https://cluster.example/openid/v1/jwks", KubernetesCAFile: "/ca.pem", KubernetesAPITokenFile: "/api-token", KubernetesNamespace: "finassist",
	}}, Peers: map[string]string{"user": "dns:///user:9090"}}
}

func TestUnitValidateIdentity(t *testing.T) {
	if errs := validIdentityConfig().validateIdentity(); len(errs) != 0 {
		t.Fatal(errs)
	}
	tests := []struct {
		name   string
		change func(*Config)
	}{
		{"AUTH_USER_ISSUER", func(c *Config) { c.Auth.Identity.UserIssuer = "" }},
		{"AUTH_USER_AUDIENCE", func(c *Config) { c.Auth.Identity.UserAudience = " " }},
		{"AUTH_KUBERNETES_ISSUER", func(c *Config) { c.Auth.Identity.KubernetesIssuer = "" }},
		{"AUTH_KUBERNETES_JWKS_URL", func(c *Config) { c.Auth.Identity.KubernetesJWKSURL = "" }},
		{"AUTH_KUBERNETES_CA_FILE", func(c *Config) { c.Auth.Identity.KubernetesCAFile = "" }},
		{"AUTH_KUBERNETES_API_TOKEN_FILE", func(c *Config) { c.Auth.Identity.KubernetesAPITokenFile = "" }},
		{"AUTH_KUBERNETES_NAMESPACE", func(c *Config) { c.Auth.Identity.KubernetesNamespace = "" }},
		{"SERVICE_TOKEN_FILE", func(c *Config) { c.Auth.ServiceTokenFile = "" }},
		{"PEER_USER_ADDR", func(c *Config) { delete(c.Peers, "user") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validIdentityConfig()
			tt.change(&c)
			errs := c.validateIdentity()
			if len(errs) != 1 || !strings.Contains(errs[0].Error(), tt.name) {
				t.Fatalf("got %v", errs)
			}
		})
	}
	c := Config{Auth: AuthConfig{Mode: AuthInsecureDev}}
	if errs := c.validateIdentity(); len(errs) != 0 {
		t.Fatal(errs)
	}
}

func validStageConfig() Config {
	return Config{Env: EnvStage, Auth: AuthConfig{Mode: AuthVerify, Identity: IdentityConfig{
		UserIssuer: "https://user.example", UserAudience: "finassist",
		StageIssuer: "https://stage.example", StageSigningKeyFile: "/stage.pem", StagePublicKeysFile: "/stage-jwks.json",
	}}, Peers: map[string]string{"user": "dns:///user:9090"}}
}

// Stage needs only its key files; the Kubernetes settings and the projected
// token are not required there.
func TestUnitValidateStageIdentity(t *testing.T) {
	if errs := validStageConfig().validateIdentity(); len(errs) != 0 {
		t.Fatal(errs)
	}
	for _, tt := range []struct {
		name   string
		change func(*Config)
	}{
		{"AUTH_STAGE_ISSUER", func(c *Config) { c.Auth.Identity.StageIssuer = " " }},
		{"AUTH_STAGE_SIGNING_KEY_FILE", func(c *Config) { c.Auth.Identity.StageSigningKeyFile = "" }},
		{"AUTH_STAGE_PUBLIC_KEYS_FILE", func(c *Config) { c.Auth.Identity.StagePublicKeysFile = "" }},
		{"AUTH_USER_ISSUER", func(c *Config) { c.Auth.Identity.UserIssuer = "" }},
		{"PEER_USER_ADDR", func(c *Config) { delete(c.Peers, "user") }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := validStageConfig()
			tt.change(&c)
			errs := c.validateIdentity()
			if len(errs) != 1 || !strings.Contains(errs[0].Error(), tt.name) || !strings.Contains(errs[0].Error(), "ENV=stage") {
				t.Fatalf("got %v", errs)
			}
		})
	}
}

// Stage keys in prod would be a second, weaker trust root; verify in dev or an
// unknown environment has no go-identity mode at all.
func TestUnitValidateIdentityEnvironmentBoundaries(t *testing.T) {
	for _, name := range []string{"AUTH_STAGE_ISSUER", "AUTH_STAGE_SIGNING_KEY_FILE", "AUTH_STAGE_PUBLIC_KEYS_FILE"} {
		t.Run(name, func(t *testing.T) {
			c := validIdentityConfig()
			switch name {
			case "AUTH_STAGE_ISSUER":
				c.Auth.Identity.StageIssuer = "https://stage.example"
			case "AUTH_STAGE_SIGNING_KEY_FILE":
				c.Auth.Identity.StageSigningKeyFile = "/stage.pem"
			default:
				c.Auth.Identity.StagePublicKeysFile = "/stage-jwks.json"
			}
			errs := c.validateIdentity()
			if len(errs) != 1 || !strings.Contains(errs[0].Error(), name) || !strings.Contains(errs[0].Error(), "only with ENV=stage") {
				t.Fatalf("stage key accepted in prod: %v", errs)
			}
		})
	}
	for _, env := range []Env{EnvDev, ""} {
		c := validIdentityConfig()
		c.Env = env
		if errs := c.validateIdentity(); len(errs) != 1 || !strings.Contains(errs[0].Error(), "ENV=prod or ENV=stage") {
			t.Fatalf("verify accepted with ENV=%q: %v", env, errs)
		}
	}
	// Load reports it too: the default AUTH_MODE=verify cannot start a dev process.
	vars := map[string]string{"SERVICE_NAME": "assistant", "ENV": "dev", "DB_PRIMARY_DSN": "postgres://primary/assistant", "AUTH_USER_ISSUER": "https://user.example", "AUTH_USER_AUDIENCE": "finassist", "PEER_USER_ADDR": "dns:///user:9090"}
	get, list := env(vars)
	if _, err := load(get, noFiles, list); err == nil || !strings.Contains(err.Error(), "ENV=prod or ENV=stage") {
		t.Fatalf("verify in dev loaded: %v", err)
	}
}

func TestUnitValidateIdentityJWKSURL(t *testing.T) {
	for _, address := range []string{"http://cluster/jwks", "https://name:secret@cluster/jwks", "https://cluster/jwks?token=secret", "https://cluster/jwks#key", "https:///jwks", ":invalid"} {
		t.Run(address, func(t *testing.T) {
			c := validIdentityConfig()
			c.Auth.Identity.KubernetesJWKSURL = address
			errs := c.validateIdentity()
			if len(errs) != 1 || strings.Contains(errs[0].Error(), "secret") {
				t.Fatalf("unsafe URL accepted or leaked: %v", errs)
			}
		})
	}
	c := validIdentityConfig()
	c.Auth.Identity.KubernetesAPITokenFile = c.Auth.ServiceTokenFile
	if errs := c.validateIdentity(); len(errs) != 1 {
		t.Fatalf("shared token file accepted: %v", errs)
	}
}

func TestUnitReadIdentity(t *testing.T) {
	vars := map[string]string{
		"AUTH_USER_ISSUER": "https://user.example", "AUTH_USER_AUDIENCE": "mobile",
		"AUTH_KUBERNETES_ISSUER": "https://cluster.example", "AUTH_KUBERNETES_JWKS_URL": "https://cluster.example/keys",
		"AUTH_KUBERNETES_CA_FILE": "/ca", "AUTH_KUBERNETES_API_TOKEN_FILE": "/token", "AUTH_KUBERNETES_NAMESPACE": "isolated",
		"AUTH_STAGE_ISSUER": "https://stage.example", "AUTH_STAGE_SIGNING_KEY_FILE": "/stage.pem", "AUTH_STAGE_PUBLIC_KEYS_FILE": "/stage-jwks.json",
	}
	r := reader{getenv: func(k string) string { return vars[k] }, command: "serve"}
	got := r.identity()
	want := IdentityConfig{UserIssuer: vars["AUTH_USER_ISSUER"], UserAudience: "mobile", KubernetesIssuer: vars["AUTH_KUBERNETES_ISSUER"], KubernetesJWKSURL: vars["AUTH_KUBERNETES_JWKS_URL"], KubernetesCAFile: "/ca", KubernetesAPITokenFile: "/token", KubernetesNamespace: "isolated",
		StageIssuer: "https://stage.example", StageSigningKeyFile: "/stage.pem", StagePublicKeysFile: "/stage-jwks.json"}
	if got != want || len(r.errs) != 0 {
		t.Fatalf("got %+v, %v", got, r.errs)
	}
}
