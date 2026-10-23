package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// User - пользователь бота, привязан к Telegram-аккаунту
type User struct {
	ID         int64
	TelegramID int64
	CreatedAt  time.Time
}

// EnsureUser возвращает пользователя по Telegram ID, создавая его вместе с настройками по умолчанию
func (s *Store) EnsureUser(ctx context.Context, telegramID int64) (User, error) {
	var u User
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO users (telegram_id) VALUES (?) ON CONFLICT (telegram_id) DO NOTHING`, telegramID)
		if err != nil {
			return fmt.Errorf("insert user: %w", err)
		}

		u, err = scanUser(tx.QueryRowContext(ctx,
			`SELECT id, telegram_id, created_at FROM users WHERE telegram_id = ?`, telegramID))
		if err != nil {
			return err
		}

		_, err = tx.ExecContext(ctx,
			`INSERT INTO user_settings (user_id) VALUES (?) ON CONFLICT (user_id) DO NOTHING`, u.ID)
		if err != nil {
			return fmt.Errorf("insert default settings: %w", err)
		}
		return nil
	})
	return u, err
}

// UserByTelegramID ищет пользователя по Telegram ID, ErrNotFound если такого нет
func (s *Store) UserByTelegramID(ctx context.Context, telegramID int64) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx,
		`SELECT id, telegram_id, created_at FROM users WHERE telegram_id = ?`, telegramID))
}

// Users возвращает всех пользователей, например для обхода напоминаниями
func (s *Store) Users(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, telegram_id, created_at FROM users ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("query users: %w", err)
	}
	// Ошибки чтения ловит rows.Err ниже, у Close после полного обхода ловить нечего
	defer func() { _ = rows.Close() }()

	var users []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate users: %w", err)
	}
	return users, nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanUser(row scanner) (User, error) {
	var (
		u         User
		createdAt string
	)
	if err := row.Scan(&u.ID, &u.TelegramID, &createdAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return User{}, ErrNotFound
		}
		return User{}, fmt.Errorf("scan user: %w", err)
	}

	var err error
	if u.CreatedAt, err = parseTime(createdAt); err != nil {
		return User{}, err
	}
	return u, nil
}
