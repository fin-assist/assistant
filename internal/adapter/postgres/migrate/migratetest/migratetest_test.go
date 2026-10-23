package migratetest

import (
	"testing"
	"testing/fstest"
)

func TestDownFilesAcceptsLibraryAndServicePadding(t *testing.T) {
	for _, name := range []string{"001_users.down.sql", "000001_users.down.sql"} {
		files := fstest.MapFS{name: {Data: []byte("DROP TABLE x;")}, "002_next.down.sql": {}, "notes.down.sql": {}, "invalid_name.down.sql": {}}
		matches, err := downFiles(files, 1)
		if err != nil || len(matches) != 1 || matches[0] != name {
			t.Fatalf("%v %v", matches, err)
		}
		if matches, err := downFiles(files, 3); err != nil || len(matches) != 0 {
			t.Fatalf("%v %v", matches, err)
		}
	}
	files := fstest.MapFS{"001_first.down.sql": {}, "000001_duplicate.down.sql": {}}
	matches, err := downFiles(files, 1)
	if err != nil || len(matches) != 2 {
		t.Fatalf("ambiguous files must reach validation: %v %v", matches, err)
	}
}
