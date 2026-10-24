package migrate

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"
)

func TestUnitMigrationCommandsValidateSourcesBeforeDatabaseAccess(t *testing.T) {
	cfg := DefaultConfig("not a valid DSN")
	good := Source{Name: "service", Table: "schema_migrations", FS: fstest.MapFS{}}
	for _, tc := range []struct {
		name    string
		sources []Source
		want    string
	}{
		{"name", []Source{{Table: good.Table, FS: good.FS}}, "required"},
		{"table", []Source{{Name: good.Name, FS: good.FS}}, "required"},
		{"files", []Source{{Name: good.Name, Table: good.Table}}, "required"},
		{"duplicate-name", []Source{good, {Name: good.Name, Table: "other", FS: good.FS}}, "listed twice"},
		{"duplicate-table", []Source{good, {Name: "other", Table: good.Table, FS: good.FS}}, "used by two sources"},
		{"invalid-file", []Source{{Name: good.Name, Table: good.Table, FS: fstest.MapFS{"broken.sql": {Data: []byte("SELECT 1")}}}}, "does not match"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for name, call := range map[string]func() error{
				"up":      func() error { return Up(t.Context(), cfg, tc.sources) },
				"version": func() error { _, err := Versions(t.Context(), cfg, tc.sources); return err },
			} {
				if err := call(); err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("%s ignored source validation: %v", name, err)
				}
			}
		})
	}
	for name, call := range map[string]func(context.Context) error{
		"down":  func(ctx context.Context) error { return Down(ctx, cfg, Source{}, 1) },
		"force": func(ctx context.Context) error { return Force(ctx, cfg, Source{}, 1) },
	} {
		if err := call(t.Context()); err == nil || !strings.Contains(err.Error(), "required") {
			t.Fatalf("%s invalid source=%v", name, err)
		}
	}
	for _, steps := range []int{0, -1} {
		if err := Down(t.Context(), cfg, good, steps); err == nil || !strings.Contains(err.Error(), "positive") {
			t.Fatalf("nonpositive rollback=%v", err)
		}
	}
}
