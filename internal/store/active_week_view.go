package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ActiveWeekView - сообщение о текущей неделе в личке, включая ожидание нового поста
type ActiveWeekView struct {
	MessageID int
	PostID    int64
	WeekStart time.Time
	Problems  string
	Submit    bool
	Header    string
}

func (s *Store) ActiveWeekView(ctx context.Context, userID int64) (ActiveWeekView, error) {
	var view ActiveWeekView
	var weekStart string
	err := s.db.QueryRowContext(ctx, `
		SELECT message_id, COALESCE(post_id, 0), week_start, problems, submit, header
		FROM active_week_views WHERE user_id = ?`, userID).
		Scan(&view.MessageID, &view.PostID, &weekStart, &view.Problems, &view.Submit, &view.Header)
	if errors.Is(err, sql.ErrNoRows) {
		return ActiveWeekView{}, nil
	}
	if err != nil {
		return ActiveWeekView{}, fmt.Errorf("load active week view: %w", err)
	}
	view.WeekStart, err = parseDate(weekStart)
	return view, err
}

func (s *Store) SaveActiveWeekView(ctx context.Context, userID int64, view ActiveWeekView) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO active_week_views (user_id, message_id, post_id, week_start, problems, submit, header)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (user_id) DO UPDATE SET message_id = excluded.message_id, post_id = excluded.post_id,
			week_start = excluded.week_start, problems = excluded.problems, submit = excluded.submit, header = excluded.header`,
		userID, view.MessageID, nullID(view.PostID), formatDate(view.WeekStart), view.Problems, boolToInt(view.Submit), view.Header)
	if err != nil {
		return fmt.Errorf("save active week view: %w", err)
	}
	return nil
}

func (s *Store) DeleteActiveWeekView(ctx context.Context, userID int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM active_week_views WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("delete active week view: %w", err)
	}
	return nil
}
