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
	WeekStart time.Time // неделя на момент принятия поста, у старых записей может отсутствовать

	ArchiveText       string // сохранённый итог подтверждённой недели
	BotButtonBase     string // клавиатура канала без кнопки бота
	BotButtonArchived bool   // последняя установленная кнопка открывает итог
}

const weekPostColumns = `id, user_id, chat_id, message_id, text, posted_at, edited_at, week_start,
	COALESCE(archive_text, ''), COALESCE(bot_button_base, ''), bot_button_archived`

// SaveWeekPost сохраняет новый пост или обновляет текст уже известного (по chat_id и message_id).
// Апдейты Telegram обрабатываются параллельно и могут прийти не по порядку, поэтому текст обновляется,
// только если правка не старше сохранённой. updated = false - пришла устаревшая версия, в базе осталась прежняя,
// и вернулась именно она
func (s *Store) SaveWeekPost(ctx context.Context, p WeekPost) (saved WeekPost, updated bool, err error) {
	var weekStart sql.NullString
	if !p.WeekStart.IsZero() {
		weekStart = sql.NullString{String: formatDate(p.WeekStart), Valid: true}
	}
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO week_posts (user_id, chat_id, message_id, text, posted_at, edited_at, week_start)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (chat_id, message_id) DO UPDATE SET text = excluded.text, edited_at = excluded.edited_at
		WHERE excluded.edited_at >= COALESCE(week_posts.edited_at, '')
		RETURNING id, week_start, COALESCE(archive_text, ''), COALESCE(bot_button_base, ''), bot_button_archived`,
		p.UserID, p.ChatID, p.MessageID, p.Text, formatTime(p.PostedAt), nullTime(p.EditedAt), weekStart,
	).Scan(&p.ID, &weekStart, &p.ArchiveText, &p.BotButtonBase, &p.BotButtonArchived)
	if errors.Is(err, sql.ErrNoRows) {
		current, err := s.WeekPostByMessage(ctx, p.ChatID, p.MessageID)
		return current, false, err
	}
	if err != nil {
		return WeekPost{}, false, fmt.Errorf("upsert week post: %w", err)
	}
	p.WeekStart, err = parseNullDate(weekStart)
	if err != nil {
		return WeekPost{}, false, err
	}
	return p, true, nil
}

// WeekPostByMessage ищет сохранённый пост по сообщению в канале, ErrNotFound если такого нет
func (s *Store) WeekPostByMessage(ctx context.Context, chatID, messageID int64) (WeekPost, error) {
	return scanWeekPost(s.db.QueryRowContext(ctx, `
		SELECT `+weekPostColumns+`
		FROM week_posts WHERE chat_id = ? AND message_id = ?`, chatID, messageID))
}

// LatestWeekPost возвращает самый свежий пост пользователя, ErrNotFound если постов нет
func (s *Store) LatestWeekPost(ctx context.Context, userID int64) (WeekPost, error) {
	return scanWeekPost(s.db.QueryRowContext(ctx, `
		SELECT `+weekPostColumns+`
		FROM week_posts WHERE user_id = ?
		ORDER BY posted_at DESC, id DESC LIMIT 1`, userID))
}

// LatestWeekPostInChannel возвращает последний известный пост из указанного канала
func (s *Store) LatestWeekPostInChannel(ctx context.Context, userID, chatID int64) (WeekPost, error) {
	return scanWeekPost(s.db.QueryRowContext(ctx, `
		SELECT `+weekPostColumns+`
		FROM week_posts WHERE user_id = ? AND chat_id = ?
		ORDER BY posted_at DESC, id DESC LIMIT 1`, userID, chatID))
}

// WeekPostForWeek ищет пост за календарную неделю; для старых постов используется дата публикации
func (s *Store) WeekPostForWeek(ctx context.Context, userID int64, monday time.Time) (WeekPost, error) {
	return scanWeekPost(s.db.QueryRowContext(ctx, `
		SELECT `+weekPostColumns+`
		FROM week_posts WHERE user_id = ? AND (
			week_start = ? OR (week_start IS NULL AND posted_at >= ? AND posted_at < ?)
		)
		ORDER BY week_start DESC, posted_at DESC, id DESC LIMIT 1`,
		userID, formatDate(monday), formatTime(monday), formatTime(monday.AddDate(0, 0, 7))))
}

// WeekArchive возвращает сохранённый итог поста из канала пользователя
func (s *Store) WeekArchive(ctx context.Context, userID, chatID, postID int64) (WeekPost, error) {
	return scanWeekPost(s.db.QueryRowContext(ctx, `
		SELECT `+weekPostColumns+`
		FROM week_posts WHERE id = ? AND user_id = ? AND chat_id = ? AND archive_text IS NOT NULL`,
		postID, userID, chatID))
}

// SaveWeekArchive обновляет итог, пустой текст возвращает пост в работу
func (s *Store) SaveWeekArchive(ctx context.Context, postID int64, text string) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE week_posts SET archive_text = NULLIF(?, '') WHERE id = ?`, text, postID); err != nil {
		return fmt.Errorf("save week archive: %w", err)
	}
	return nil
}

func scanWeekPost(row scanner) (WeekPost, error) {
	var (
		p         WeekPost
		postedAt  string
		editedAt  sql.NullString
		weekStart sql.NullString
	)
	err := row.Scan(&p.ID, &p.UserID, &p.ChatID, &p.MessageID, &p.Text, &postedAt, &editedAt, &weekStart,
		&p.ArchiveText, &p.BotButtonBase, &p.BotButtonArchived)
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
	if p.WeekStart, err = parseNullDate(weekStart); err != nil {
		return WeekPost{}, err
	}
	return p, nil
}

func parseNullDate(s sql.NullString) (time.Time, error) {
	if !s.Valid {
		return time.Time{}, nil
	}
	return parseDate(s.String)
}

// ChannelBotButton - клавиатура поста без кнопки возврата в бота
type ChannelBotButton struct {
	PostID    int64
	ChatID    int64
	MessageID int64
	Base      string
}

// WorkingChannelBotButtons возвращает обычные кнопки под постами, для которых ещё нет итога
func (s *Store) WorkingChannelBotButtons(ctx context.Context, userID, chatID int64, weekStart time.Time) ([]ChannelBotButton, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, chat_id, message_id, bot_button_base FROM week_posts
		WHERE user_id = ? AND chat_id = ? AND bot_button_base IS NOT NULL
		      AND archive_text IS NULL AND bot_button_archived = 0
		      AND (week_start < ? OR (week_start IS NULL AND posted_at < ?))`,
		userID, chatID, formatDate(weekStart), formatTime(weekStart))
	if err != nil {
		return nil, fmt.Errorf("list channel bot buttons: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var buttons []ChannelBotButton
	for rows.Next() {
		var button ChannelBotButton
		if err := rows.Scan(&button.PostID, &button.ChatID, &button.MessageID, &button.Base); err != nil {
			return nil, fmt.Errorf("scan channel bot button: %w", err)
		}
		buttons = append(buttons, button)
	}
	return buttons, rows.Err()
}

func (s *Store) SetChannelBotButton(ctx context.Context, postID int64, base string, archived bool) error {
	if _, err := s.db.ExecContext(ctx, `
		UPDATE week_posts SET bot_button_base = NULLIF(?, ''), bot_button_archived = ? WHERE id = ?`,
		base, archived, postID); err != nil {
		return fmt.Errorf("set channel bot button: %w", err)
	}
	return nil
}
