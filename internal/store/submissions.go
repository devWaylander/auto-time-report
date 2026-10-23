package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Submission - успешно записанный в Jira worklog для пары «задача + дата»
type Submission struct {
	ID         int64
	UserID     int64
	WeekPostID int64 // 0 - пост неизвестен или удалён
	IssueKey   string
	WorkDate   time.Time
	Duration   time.Duration
	Comment    string
	WorklogID  int64
	PeriodID   int64
	CreatedAt  time.Time
}

// SaveSubmission сохраняет запись о worklog'е; повтор той же пары «задача + дата» - ошибка
func (s *Store) SaveSubmission(ctx context.Context, sub Submission) (Submission, error) {
	var (
		weekPostID sql.NullInt64
		createdAt  string
	)
	if sub.WeekPostID != 0 {
		weekPostID = sql.NullInt64{Int64: sub.WeekPostID, Valid: true}
	}

	err := s.db.QueryRowContext(ctx, `
		INSERT INTO submissions
		    (user_id, week_post_id, issue_key, work_date, duration_seconds, comment, worklog_id, period_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		RETURNING id, created_at`,
		sub.UserID, weekPostID, sub.IssueKey, formatDate(sub.WorkDate), int64(sub.Duration/time.Second),
		sub.Comment, sub.WorklogID, sub.PeriodID,
	).Scan(&sub.ID, &createdAt)
	if err != nil {
		return Submission{}, fmt.Errorf("insert submission: %w", err)
	}

	if sub.CreatedAt, err = parseTime(createdAt); err != nil {
		return Submission{}, err
	}
	return sub, nil
}

// SubmissionsBetween возвращает записи пользователя за даты from..to включительно
func (s *Store) SubmissionsBetween(ctx context.Context, userID int64, from, to time.Time) ([]Submission, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, user_id, week_post_id, issue_key, work_date, duration_seconds, comment,
		       worklog_id, period_id, created_at
		FROM submissions
		WHERE user_id = ? AND work_date BETWEEN ? AND ?
		ORDER BY work_date, issue_key`,
		userID, formatDate(from), formatDate(to))
	if err != nil {
		return nil, fmt.Errorf("query submissions: %w", err)
	}
	// Ошибки чтения ловит rows.Err ниже, у Close после полного обхода ловить нечего
	defer func() { _ = rows.Close() }()

	var subs []Submission
	for rows.Next() {
		var (
			sub                 Submission
			weekPostID          sql.NullInt64
			workDate, createdAt string
			seconds             int64
		)
		err := rows.Scan(&sub.ID, &sub.UserID, &weekPostID, &sub.IssueKey, &workDate, &seconds, &sub.Comment,
			&sub.WorklogID, &sub.PeriodID, &createdAt)
		if err != nil {
			return nil, fmt.Errorf("scan submission: %w", err)
		}

		sub.WeekPostID = weekPostID.Int64
		sub.Duration = time.Duration(seconds) * time.Second
		if sub.WorkDate, err = parseDate(workDate); err != nil {
			return nil, err
		}
		if sub.CreatedAt, err = parseTime(createdAt); err != nil {
			return nil, err
		}
		subs = append(subs, sub)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate submissions: %w", err)
	}
	return subs, nil
}

// MarkPeriodRequested отмечает, что период отправлен на проверку; повторная отправка обновляет время
func (s *Store) MarkPeriodRequested(ctx context.Context, userID, periodID int64) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO period_approvals (user_id, period_id) VALUES (?, ?)
		ON CONFLICT (user_id, period_id) DO UPDATE SET requested_at = excluded.requested_at`,
		userID, periodID)
	if err != nil {
		return fmt.Errorf("upsert period approval: %w", err)
	}
	return nil
}

// PeriodRequested сообщает, отправлялся ли период на проверку
func (s *Store) PeriodRequested(ctx context.Context, userID, periodID int64) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM period_approvals WHERE user_id = ? AND period_id = ?)`,
		userID, periodID,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("query period approval: %w", err)
	}
	return exists, nil
}
