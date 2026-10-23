package config

import (
	"strings"
	"testing"
)

func validIdentityConfig() Config {
	return Config{Auth: AuthConfig{Mode: AuthVerify, ServiceTokenFile: "/internal-token", Identity: IdentityConfig{
		UserIssuer: "https://user.example", UserAudience: "finassist", KubernetesIssuer: "https://cluster.example", KubernetesJWKSURL: "https://cluster.example/openid/v1/jwks", KubernetesCAFile: "/ca.pem", KubernetesAPITokenFile: "/api-token", KubernetesNamespace: "finassist",
	}}, Peers: map[string]string{"user": "dns:///user:9090"}}
}

func TestValidateIdentity(t *testing.T) {
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

func TestValidateIdentityJWKSURL(t *testing.T) {
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

func TestReadIdentity(t *testing.T) {
	vars := map[string]string{
		"AUTH_USER_ISSUER": "https://user.example", "AUTH_USER_AUDIENCE": "mobile",
		"AUTH_KUBERNETES_ISSUER": "https://cluster.example", "AUTH_KUBERNETES_JWKS_URL": "https://cluster.example/keys",
		"AUTH_KUBERNETES_CA_FILE": "/ca", "AUTH_KUBERNETES_API_TOKEN_FILE": "/token", "AUTH_KUBERNETES_NAMESPACE": "isolated",
	}
	r := reader{getenv: func(k string) string { return vars[k] }, command: "serve"}
	got := r.identity()
	want := IdentityConfig{UserIssuer: vars["AUTH_USER_ISSUER"], UserAudience: "mobile", KubernetesIssuer: vars["AUTH_KUBERNETES_ISSUER"], KubernetesJWKSURL: vars["AUTH_KUBERNETES_JWKS_URL"], KubernetesCAFile: "/ca", KubernetesAPITokenFile: "/token", KubernetesNamespace: "isolated"}
	if got != want || len(r.errs) != 0 {
		t.Fatalf("got %+v, %v", got, r.errs)
	}
}
