package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/fin-assist/assistant/internal/testdb"
)

func TestIntegrationMigrationCommandsPreserveIndependentHistories(t *testing.T) {
	dsn := testdb.DSN(t)
	t.Setenv("DB_PRIMARY_DSN", dsn)
	if err := run(t.Context(), []string{"migrate", "up"}); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(t.Context()) })
	checkTable := func(table string, want bool) {
		t.Helper()
		var exists bool
		if err := conn.QueryRow(t.Context(), `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists); err != nil || exists != want {
			t.Fatalf("table%s exists=%v want%v err=%v", table, exists, want, err)
		}
	}
	for _, table := range []string{"user_state", "question", "off_topic_event", "tombstone_schema_migrations", "idempotency_schema_migrations"} {
		checkTable(table, true)
	}
	checkRevisions := func(want bool) {
		t.Helper()
		var exists bool
		if err := conn.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_attribute WHERE attrelid='answer'::regclass AND attname='data_revisions' AND NOT attisdropped)`).Scan(&exists); err != nil || exists != want {
			t.Fatalf("revision column: %v want %v %v", exists, want, err)
		}
	}
	checkRevisions(true)
	var output bytes.Buffer
	if err := runMigrate(t.Context(), []string{"version"}, &output); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"service\t3\n", "tombstone\t1\n", "idempotency\t5\n"} {
		if !strings.Contains(output.String(), line) {
			t.Fatalf("versionmissing%q in%q", line, output.String())
		}
	}
	if err := runMigrate(t.Context(), []string{"down", "service", "1"}, &output); err != nil {
		t.Fatal(err)
	}
	checkRevisions(false)
	checkTable("off_topic_event", true)
	if err := runMigrate(t.Context(), []string{"down", "service", "1"}, &output); err != nil {
		t.Fatal(err)
	}
	checkTable("off_topic_event", false)
	checkTable("question", true)
	checkTable("tombstone_schema_migrations", true)
	checkTable("idempotency_schema_migrations", true)
	if err := runMigrate(t.Context(), []string{"up"}, &output); err != nil {
		t.Fatal(err)
	}
	checkTable("off_topic_event", true)
	checkRevisions(true)
	if _, err := conn.Exec(t.Context(), `UPDATE schema_migrations SET dirty=true`); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := runMigrate(t.Context(), []string{"version"}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "service\t3 (dirty)") {
		t.Fatalf("dirtyversion=%q", output.String())
	}
	if err := runMigrate(t.Context(), []string{"force", "service", "3"}, &output); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := runMigrate(t.Context(), []string{"version"}, &output); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "dirty") || !strings.Contains(output.String(), "service\t3\n") {
		t.Fatalf("forcedversion=%q", output.String())
	}
	if err := runMigrate(t.Context(), []string{"up"}, &output); err != nil {
		t.Fatalf("idempotentup=%v", err)
	}
	checkTable("off_topic_event", true)
}
