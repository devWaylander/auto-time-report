// Package storetest готовит базу для тестов: накатывает настоящие миграции из db/migrations так же, как dbmate
package storetest

import (
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// NewDatabaseURL создаёт базу во временной папке теста и возвращает её DATABASE_URL
func NewDatabaseURL(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()

	if _, err := db.Exec(`CREATE TABLE schema_migrations (version TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}

	files, err := filepath.Glob(filepath.Join(migrationsDir(t), "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no migrations found")
	}
	sort.Strings(files)

	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		up, _, ok := strings.Cut(string(body), "-- migrate:down")
		if !ok {
			t.Fatalf("%s: no migrate:down section", f)
		}
		if _, err := db.Exec(up); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		version, _, _ := strings.Cut(filepath.Base(f), "_")
		if _, err := db.Exec(`INSERT INTO schema_migrations (version) VALUES (?)`, version); err != nil {
			t.Fatal(err)
		}
	}
	return "sqlite:" + path
}

// migrationsDir находит db/migrations относительно этого файла, откуда бы ни запускались тесты
func migrationsDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate storetest source")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "db", "migrations")
}
