package main

import (
	"errors"
	"strings"
	"testing"
)

func TestUnitCommandDispatchRejectsInvalidUsageBeforeStartup(t *testing.T) {
	for _, args := range [][]string{nil, {"unknown"}, {"serve", "extra"}, {"migrate"}} {
		err := run(t.Context(), args)
		var usageErr usageError
		if !errors.As(err, &usageErr) || !strings.Contains(err.Error(), "usage:") {
			t.Fatalf("args%v error%v", args, err)
		}
	}
}

func TestUnitCommandDispatchPreservesConfigurationErrors(t *testing.T) {
	t.Setenv("DB_PRIMARY_DSN", "")
	t.Setenv("DB_PRIMARY_DSN_FILE", "")
	t.Setenv("ENV", "prod")
	t.Setenv("AUTH_MODE", "insecure-dev")
	for _, args := range [][]string{{"serve"}, {"migrate", "up"}} {
		err := run(t.Context(), args)
		var usageErr usageError
		if err == nil || errors.As(err, &usageErr) || !strings.Contains(err.Error(), "DB_PRIMARY_DSN") {
			t.Fatalf("args%v hid configuration failure: %v", args, err)
		}
	}
}
