package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// WeekPost - недельный пост из канала в последней известной версии
type WeekPost struct {
	ID        int64
	UserID    int64
	ChatID    int64
	MessageID int64
	Text      string
	PostedAt  time.Time
	EditedAt  time.Time // нулевое значение - пост не редактировался
}

// SaveWeekPost сохраняет новый пост или обновляет текст уже известного (по chat_id и message_id)
func (s *Store) SaveWeekPost(ctx context.Context, p WeekPost) (WeekPost, error) {
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO week_posts (user_id, chat_id, message_id, text, posted_at, edited_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (chat_id, message_id) DO UPDATE SET text = excluded.text, edited_at = excluded.edited_at
		RETURNING id`,
		p.UserID, p.ChatID, p.MessageID, p.Text, formatTime(p.PostedAt), nullTime(p.EditedAt),
	).Scan(&p.ID)
	if err != nil {
		return WeekPost{}, fmt.Errorf("upsert week post: %w", err)
	}
	return p, nil
}

// LatestWeekPost возвращает самый свежий пост пользователя, ErrNotFound если постов нет
func (s *Store) LatestWeekPost(ctx context.Context, userID int64) (WeekPost, error) {
	var (
		p        WeekPost
		postedAt string
		editedAt sql.NullString
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, chat_id, message_id, text, posted_at, edited_at
		FROM week_posts WHERE user_id = ?
		ORDER BY posted_at DESC, id DESC LIMIT 1`, userID,
	).Scan(&p.ID, &p.UserID, &p.ChatID, &p.MessageID, &p.Text, &postedAt, &editedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return WeekPost{}, ErrNotFound
	}
	if err != nil {
		return WeekPost{}, fmt.Errorf("query week post: %w", err)
	}

	if p.PostedAt, err = parseTime(postedAt); err != nil {
		return WeekPost{}, err
	}
	if p.EditedAt, err = parseNullTime(editedAt); err != nil {
		return WeekPost{}, err
	}
	return p, nil
}
