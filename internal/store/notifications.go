package store

import (
	"context"
	"fmt"
	"time"
)

// NotificationKind — тип напоминания
type NotificationKind string

const (
	NotificationDaily  NotificationKind = "daily"
	NotificationWeekly NotificationKind = "weekly"
)

// ClaimNotification атомарно занимает напоминание на дату: true — его ещё не отправляли и слать нужно,
// false — уже отправлено, в том числе до рестарта бота. Занимать нужно до отправки,
// поэтому упавшая отправка повторно не уйдёт
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
