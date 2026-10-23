// Package store хранит состояние бота в SQLite. Схемой владеют миграции dbmate в db/migrations,
// сам пакет таблицы не создаёт
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound возвращается, когда запрошенной записи нет
var ErrNotFound = errors.New("not found")

const (
	dateLayout = "2006-01-02"
	timeLayout = time.RFC3339
)

// Store - доступ к базе бота, безопасен для использования из нескольких горутин
type Store struct {
	db *sql.DB
}

// Open подключается к базе по DATABASE_URL в формате dbmate (sqlite:./data/bot.db)
// и проверяет, что миграции уже накатаны
func Open(ctx context.Context, databaseURL string) (*Store, error) {
	path, err := pathFromURL(databaseURL)
	if err != nil {
		return nil, err
	}

	// _txlock=immediate берёт блокировку записи в начале транзакции,
	// иначе параллельные транзакции ловят SQLITE_BUSY при апгрейде блокировки
	dsn := "file:" + path + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_txlock=immediate"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	s := &Store{db: db}
	if err := s.checkMigrated(ctx); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return s, nil
}

// Close закрывает соединения с базой
func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) checkMigrated(ctx context.Context) error {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&n)
	if err != nil {
		return fmt.Errorf("check migrations (run `make migrate-up`): %w", err)
	}
	if n == 0 {
		return errors.New("no migrations applied, run `make migrate-up`")
	}
	return nil
}

// pathFromURL разбирает URL так же, как dbmate: sqlite:relative/path, sqlite:/abs/path, sqlite:///abs/path
func pathFromURL(databaseURL string) (string, error) {
	u, err := url.Parse(databaseURL)
	if err != nil {
		return "", fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	if u.Scheme != "sqlite" && u.Scheme != "sqlite3" {
		return "", fmt.Errorf("DATABASE_URL: unsupported scheme %q, want sqlite", u.Scheme)
	}

	path := u.Opaque
	if path == "" {
		path = u.Path
	}
	if strings.TrimSpace(path) == "" {
		return "", errors.New("DATABASE_URL: empty database path")
	}
	return path, nil
}

func (s *Store) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	if err := fn(tx); err != nil {
		return errors.Join(err, tx.Rollback())
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}

// Даты хранятся как календарные (YYYY-MM-DD): при записи берётся дата в локации значения,
// при чтении возвращается полночь UTC
func formatDate(t time.Time) string {
	return t.Format(dateLayout)
}

func parseDate(s string) (time.Time, error) {
	t, err := time.Parse(dateLayout, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse date %q: %w", s, err)
	}
	return t, nil
}

func formatTime(t time.Time) string {
	return t.UTC().Format(timeLayout)
}

func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(timeLayout, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse time %q: %w", s, err)
	}
	return t, nil
}

// nullTime превращает нулевое время в NULL
func nullTime(t time.Time) sql.NullString {
	if t.IsZero() {
		return sql.NullString{}
	}
	return sql.NullString{String: formatTime(t), Valid: true}
}

func parseNullTime(s sql.NullString) (time.Time, error) {
	if !s.Valid {
		return time.Time{}, nil
	}
	return parseTime(s.String)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
