package config

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

// IdentityConfig pins the trusted issuers, audiences and service key source:
// Kubernetes in prod (the API token belongs to the Kubernetes API audience;
// ServiceTokenFile is the separate finassist-internal projected token) and
// signed key files in stage.
type IdentityConfig struct {
	UserIssuer             string
	UserAudience           string
	KubernetesIssuer       string
	KubernetesJWKSURL      string
	KubernetesCAFile       string
	KubernetesAPITokenFile string
	KubernetesNamespace    string
	StageIssuer            string
	StageSigningKeyFile    string
	StagePublicKeysFile    string
}

func (r *reader) identity() IdentityConfig {
	return IdentityConfig{
		UserIssuer:             r.str("AUTH_USER_ISSUER"),
		UserAudience:           r.str("AUTH_USER_AUDIENCE"),
		KubernetesIssuer:       r.str("AUTH_KUBERNETES_ISSUER"),
		KubernetesJWKSURL:      r.str("AUTH_KUBERNETES_JWKS_URL"),
		KubernetesCAFile:       r.str("AUTH_KUBERNETES_CA_FILE"),
		KubernetesAPITokenFile: r.str("AUTH_KUBERNETES_API_TOKEN_FILE"),
		KubernetesNamespace:    r.str("AUTH_KUBERNETES_NAMESPACE"),
		StageIssuer:            r.str("AUTH_STAGE_ISSUER"),
		StageSigningKeyFile:    r.str("AUTH_STAGE_SIGNING_KEY_FILE"),
		StagePublicKeysFile:    r.str("AUTH_STAGE_PUBLIC_KEYS_FILE"),
	}
}

func (c Config) validateIdentity() []error {
	if c.Auth.Mode != AuthVerify {
		return nil
	}
	type setting struct{ name, value string }
	required := []setting{
		{"AUTH_USER_ISSUER", c.Auth.Identity.UserIssuer},
		{"AUTH_USER_AUDIENCE", c.Auth.Identity.UserAudience},
		{"PEER_USER_ADDR", c.Peers["user"]},
	}
	stage := []setting{
		{"AUTH_STAGE_ISSUER", c.Auth.Identity.StageIssuer},
		{"AUTH_STAGE_SIGNING_KEY_FILE", c.Auth.Identity.StageSigningKeyFile},
		{"AUTH_STAGE_PUBLIC_KEYS_FILE", c.Auth.Identity.StagePublicKeysFile},
	}
	var errs []error
	switch c.Env {
	case EnvProd:
		required = append(required,
			setting{"AUTH_KUBERNETES_ISSUER", c.Auth.Identity.KubernetesIssuer},
			setting{"AUTH_KUBERNETES_JWKS_URL", c.Auth.Identity.KubernetesJWKSURL},
			setting{"AUTH_KUBERNETES_CA_FILE", c.Auth.Identity.KubernetesCAFile},
			setting{"AUTH_KUBERNETES_API_TOKEN_FILE", c.Auth.Identity.KubernetesAPITokenFile},
			setting{"AUTH_KUBERNETES_NAMESPACE", c.Auth.Identity.KubernetesNamespace},
			setting{"SERVICE_TOKEN_FILE", c.Auth.ServiceTokenFile})
		// Stage keys in prod would be a second, weaker trust root: refuse them.
		for _, v := range stage {
			if strings.TrimSpace(v.value) != "" {
				errs = append(errs, errors.New(v.name+" is allowed only with ENV=stage"))
			}
		}
		if v := c.Auth.Identity.KubernetesJWKSURL; v != "" {
			u, err := url.Parse(v)
			if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
				errs = append(errs, errors.New("AUTH_KUBERNETES_JWKS_URL must be HTTPS without credentials, query or fragment"))
			}
		}
		if c.Auth.ServiceTokenFile != "" && filepath.Clean(c.Auth.ServiceTokenFile) == filepath.Clean(c.Auth.Identity.KubernetesAPITokenFile) {
			errs = append(errs, errors.New("SERVICE_TOKEN_FILE and AUTH_KUBERNETES_API_TOKEN_FILE must use separate audiences and files"))
		}
	case EnvStage:
		required = append(required, stage...)
	default:
		// go-identity verifies only prod and stage; dev uses insecure-dev.
		return []error{fmt.Errorf("AUTH_MODE=verify requires ENV=prod or ENV=stage, not %q", c.Env)}
	}
	for _, v := range required {
		if strings.TrimSpace(v.value) == "" {
			errs = append(errs, errors.New(v.name+" is required with AUTH_MODE=verify and ENV="+string(c.Env)))
		}
	}
	return errs
}
