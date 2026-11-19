package store

import (
	"context"
	"fmt"
	"time"
)

// NotificationKind - тип напоминания
type NotificationKind string

const (
	NotificationDaily  NotificationKind = "daily"
	NotificationWeekly NotificationKind = "weekly"
)

// ClaimNotification атомарно занимает напоминание на дату: true - его ещё не отправляли и слать нужно,
// false - уже отправлено, в том числе до рестарта бота. Если отправить не удалось, отметку снимает ReleaseNotification
func (s *Store) ClaimNotification(ctx context.Context, userID int64, kind NotificationKind, date time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO notifications (user_id, kind, target_date) VALUES (?, ?, ?)
		ON CONFLICT (user_id, kind, target_date) DO NOTHING`,
		userID, string(kind), formatDate(date))
	if err != nil {
		return false, fmt.Errorf("insert notification: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("notification rows affected: %w", err)
	}
	return n == 1, nil
}

// ReleaseNotification снимает отметку, если напоминание так и не удалось отправить: следующая проверка попробует снова
func (s *Store) ReleaseNotification(ctx context.Context, userID int64, kind NotificationKind, date time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM notifications WHERE user_id = ? AND kind = ? AND target_date = ?`,
		userID, string(kind), formatDate(date))
	if err != nil {
		return fmt.Errorf("delete notification: %w", err)
	}
	return nil
}

type DailyReminderMessage struct {
	Date      time.Time
	MessageID int
}

func (s *Store) SaveDailyReminderMessage(ctx context.Context, userID int64, date time.Time, messageID int) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO notifications (user_id, kind, target_date, message_id) VALUES (?, 'daily', ?, ?)
		ON CONFLICT (user_id, kind, target_date) DO UPDATE SET message_id = excluded.message_id`,
		userID, formatDate(date), messageID)
	if err != nil {
		return fmt.Errorf("save daily reminder message: %w", err)
	}
	return nil
}

func (s *Store) DailyReminderMessages(ctx context.Context, userID int64, from, to time.Time) ([]DailyReminderMessage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT target_date, message_id FROM notifications
		WHERE user_id = ? AND kind = 'daily' AND target_date BETWEEN ? AND ? AND message_id IS NOT NULL`,
		userID, formatDate(from), formatDate(to))
	if err != nil {
		return nil, fmt.Errorf("load daily reminder messages: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var reminders []DailyReminderMessage
	for rows.Next() {
		var reminder DailyReminderMessage
		var date string
		if err := rows.Scan(&date, &reminder.MessageID); err != nil {
			return nil, fmt.Errorf("scan daily reminder message: %w", err)
		}
		reminder.Date, err = parseDate(date)
		if err != nil {
			return nil, err
		}
		reminders = append(reminders, reminder)
	}
	return reminders, rows.Err()
}

// ClearDailyReminderMessage забывает ID сообщения, сохраняя отметку об отправке
func (s *Store) ClearDailyReminderMessage(ctx context.Context, userID int64, reminder DailyReminderMessage) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE notifications SET message_id = NULL
		WHERE user_id = ? AND kind = 'daily' AND target_date = ? AND message_id = ?`,
		userID, formatDate(reminder.Date), reminder.MessageID)
	if err != nil {
		return fmt.Errorf("forget daily reminder message: %w", err)
	}
	return nil
}
