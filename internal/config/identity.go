package config

import (
	"errors"
	"net/url"
	"strings"
)

// IdentityConfig pins the trusted issuers, audiences and Kubernetes key source.
// The API token belongs to the Kubernetes API audience; ServiceTokenFile is the
// separate finassist-internal projected token.
type IdentityConfig struct {
	UserIssuer             string
	UserAudience           string
	KubernetesIssuer       string
	KubernetesJWKSURL      string
	KubernetesCAFile       string
	KubernetesAPITokenFile string
	KubernetesNamespace    string
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
	}
}

func (c Config) validateIdentity() []error {
	if c.Auth.Mode != AuthVerify {
		return nil
	}
	var errs []error
	required := []struct{ name, value string }{
		{"AUTH_USER_ISSUER", c.Auth.Identity.UserIssuer},
		{"AUTH_USER_AUDIENCE", c.Auth.Identity.UserAudience},
		{"AUTH_KUBERNETES_ISSUER", c.Auth.Identity.KubernetesIssuer},
		{"AUTH_KUBERNETES_JWKS_URL", c.Auth.Identity.KubernetesJWKSURL},
		{"AUTH_KUBERNETES_CA_FILE", c.Auth.Identity.KubernetesCAFile},
		{"AUTH_KUBERNETES_API_TOKEN_FILE", c.Auth.Identity.KubernetesAPITokenFile},
		{"AUTH_KUBERNETES_NAMESPACE", c.Auth.Identity.KubernetesNamespace},
		{"SERVICE_TOKEN_FILE", c.Auth.ServiceTokenFile},
		{"PEER_USER_ADDR", c.Peers["user"]},
	}
	for _, v := range required {
		if strings.TrimSpace(v.value) == "" {
			errs = append(errs, errors.New(v.name+" is required with AUTH_MODE=verify"))
		}
	}
	if v := c.Auth.Identity.KubernetesJWKSURL; v != "" {
		u, err := url.Parse(v)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			errs = append(errs, errors.New("AUTH_KUBERNETES_JWKS_URL must be HTTPS without credentials, query or fragment"))
		}
	}
	if c.Auth.ServiceTokenFile != "" && c.Auth.ServiceTokenFile == c.Auth.Identity.KubernetesAPITokenFile {
		errs = append(errs, errors.New("SERVICE_TOKEN_FILE and AUTH_KUBERNETES_API_TOKEN_FILE must use separate audiences and files"))
	}
	return errs
}
