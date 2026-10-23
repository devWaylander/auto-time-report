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

// SaveWeekPost сохраняет новый пост или обновляет текст уже известного (по chat_id и message_id).
// Апдейты Telegram обрабатываются параллельно и могут прийти не по порядку, поэтому текст обновляется,
// только если правка не старше сохранённой. updated = false - пришла устаревшая версия, в базе осталась прежняя,
// и вернулась именно она
func (s *Store) SaveWeekPost(ctx context.Context, p WeekPost) (saved WeekPost, updated bool, err error) {
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO week_posts (user_id, chat_id, message_id, text, posted_at, edited_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (chat_id, message_id) DO UPDATE SET text = excluded.text, edited_at = excluded.edited_at
		WHERE excluded.edited_at >= COALESCE(week_posts.edited_at, '')
		RETURNING id`,
		p.UserID, p.ChatID, p.MessageID, p.Text, formatTime(p.PostedAt), nullTime(p.EditedAt),
	).Scan(&p.ID)
	if errors.Is(err, sql.ErrNoRows) {
		current, err := s.WeekPostByMessage(ctx, p.ChatID, p.MessageID)
		return current, false, err
	}
	if err != nil {
		return WeekPost{}, false, fmt.Errorf("upsert week post: %w", err)
	}
	return p, true, nil
}

// WeekPostByMessage ищет сохранённый пост по сообщению в канале, ErrNotFound если такого нет
func (s *Store) WeekPostByMessage(ctx context.Context, chatID, messageID int64) (WeekPost, error) {
	return scanWeekPost(s.db.QueryRowContext(ctx, `
		SELECT id, user_id, chat_id, message_id, text, posted_at, edited_at
		FROM week_posts WHERE chat_id = ? AND message_id = ?`, chatID, messageID))
}

// LatestWeekPost возвращает самый свежий пост пользователя, ErrNotFound если постов нет
func (s *Store) LatestWeekPost(ctx context.Context, userID int64) (WeekPost, error) {
	return scanWeekPost(s.db.QueryRowContext(ctx, `
		SELECT id, user_id, chat_id, message_id, text, posted_at, edited_at
		FROM week_posts WHERE user_id = ?
		ORDER BY posted_at DESC, id DESC LIMIT 1`, userID))
}

func scanWeekPost(row scanner) (WeekPost, error) {
	var (
		p        WeekPost
		postedAt string
		editedAt sql.NullString
	)
	err := row.Scan(&p.ID, &p.UserID, &p.ChatID, &p.MessageID, &p.Text, &postedAt, &editedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return WeekPost{}, ErrNotFound
	}
	if err != nil {
		return WeekPost{}, fmt.Errorf("scan week post: %w", err)
	}
	if p.PostedAt, err = parseTime(postedAt); err != nil {
		return WeekPost{}, err
	}
	if p.EditedAt, err = parseNullTime(editedAt); err != nil {
		return WeekPost{}, err
	}
	return p, nil
}
