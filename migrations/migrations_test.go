package migrations_test

import (
	"testing"

	"github.com/fin-assist/assistant/internal/adapter/postgres/migrate"
	"github.com/fin-assist/assistant/internal/adapter/postgres/migrate/migratetest"
	"github.com/fin-assist/assistant/internal/app"
	"github.com/fin-assist/assistant/migrations"
)

// Every migration of the service applies, rolls back and applies again on an empty database.
func TestUpDownUp(t *testing.T) {
	src := migrate.Source{Name: "service", Table: "schema_migrations", FS: migrations.FS}
	if err := migrate.Validate(src); err != nil {
		t.Fatal(err)
	}
	if !hasMigrations(t) {
		t.Skip("the service has no migrations yet")
	}
	migratetest.CheckUpDownUp(t, src)
}

func hasMigrations(t *testing.T) bool {
	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if len(e.Name()) > 7 && e.Name()[len(e.Name())-7:] == ".up.sql" {
			return true
		}
	}
	return false
}

func TestLibraryMigrationsUpDownUp(t *testing.T) {
	for _, src := range app.MigrationSources() {
		if src.Name != "service" {
			t.Run(src.Name, func(t *testing.T) { migratetest.CheckUpDownUp(t, src) })
		}
	}
}
