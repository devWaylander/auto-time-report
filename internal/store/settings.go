package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Settings - настройки напоминаний пользователя
type Settings struct {
	UserID        int64
	DailyEnabled  bool
	DailyTime     string // ЧЧ:ММ в часовом поясе бота
	WeeklyEnabled bool
	WeeklyWeekday time.Weekday
	WeeklyTime    string    // ЧЧ:ММ в часовом поясе бота
	PausedUntil   time.Time // нулевое значение - паузы нет
	UpdatedAt     time.Time
}

// Vacation - отпуск, обе даты включительно
type Vacation struct {
	ID     int64
	UserID int64
	Start  time.Time
	End    time.Time
}

// Settings возвращает настройки пользователя
func (s *Store) Settings(ctx context.Context, userID int64) (Settings, error) {
	var (
		st                          Settings
		dailyEnabled, weeklyEnabled int
		weekday                     int
		pausedUntil                 sql.NullString
		updatedAt                   string
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT user_id, daily_enabled, daily_time, weekly_enabled, weekly_weekday, weekly_time,
		       paused_until, updated_at
		FROM user_settings WHERE user_id = ?`, userID,
	).Scan(&st.UserID, &dailyEnabled, &st.DailyTime, &weeklyEnabled, &weekday, &st.WeeklyTime,
		&pausedUntil, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Settings{}, ErrNotFound
	}
	if err != nil {
		return Settings{}, fmt.Errorf("query settings: %w", err)
	}

	st.DailyEnabled = dailyEnabled == 1
	st.WeeklyEnabled = weeklyEnabled == 1
	st.WeeklyWeekday = time.Weekday(weekday)
	if st.PausedUntil, err = parseNullTime(pausedUntil); err != nil {
		return Settings{}, err
	}
	if st.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return Settings{}, err
	}
	return st, nil
}

// UpdateSettings целиком перезаписывает настройки пользователя
func (s *Store) UpdateSettings(ctx context.Context, st Settings) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE user_settings
		SET daily_enabled = ?, daily_time = ?, weekly_enabled = ?, weekly_weekday = ?, weekly_time = ?,
		    paused_until = ?, updated_at = ?
		WHERE user_id = ?`,
		boolToInt(st.DailyEnabled), st.DailyTime, boolToInt(st.WeeklyEnabled), int(st.WeeklyWeekday),
		st.WeeklyTime, nullTime(st.PausedUntil), formatTime(time.Now()), st.UserID)
	if err != nil {
		return fmt.Errorf("update settings: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// AddVacation добавляет отпуск
func (s *Store) AddVacation(ctx context.Context, userID int64, start, end time.Time) (Vacation, error) {
	var id int64
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO vacations (user_id, start_date, end_date) VALUES (?, ?, ?) RETURNING id`,
		userID, formatDate(start), formatDate(end),
	).Scan(&id)
	if err != nil {
		return Vacation{}, fmt.Errorf("insert vacation: %w", err)
	}
	return Vacation{ID: id, UserID: userID, Start: start, End: end}, nil
}

// CancelVacations удаляет отпуска, которые ещё не закончились к дате from
func (s *Store) CancelVacations(ctx context.Context, userID int64, from time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM vacations WHERE user_id = ? AND end_date >= ?`, userID, formatDate(from))
	if err != nil {
		return fmt.Errorf("delete vacations: %w", err)
	}
	return nil
}

// UpcomingVacations возвращает отпуска, которые ещё не закончились к дате from
func (s *Store) UpcomingVacations(ctx context.Context, userID int64, from time.Time) ([]Vacation, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, start_date, end_date FROM vacations
		WHERE user_id = ? AND end_date >= ?
		ORDER BY start_date`, userID, formatDate(from))
	if err != nil {
		return nil, fmt.Errorf("query vacations: %w", err)
	}
	// Ошибки чтения ловит rows.Err ниже, у Close после полного обхода ловить нечего
	defer func() { _ = rows.Close() }()

	var vacations []Vacation
	for rows.Next() {
		var (
			v          = Vacation{UserID: userID}
			start, end string
		)
		if err := rows.Scan(&v.ID, &start, &end); err != nil {
			return nil, fmt.Errorf("scan vacation: %w", err)
		}
		if v.Start, err = parseDate(start); err != nil {
			return nil, err
		}
		if v.End, err = parseDate(end); err != nil {
			return nil, err
		}
		vacations = append(vacations, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate vacations: %w", err)
	}
	return vacations, nil
}

// OnVacation сообщает, попадает ли дата в какой-нибудь отпуск пользователя
func (s *Store) OnVacation(ctx context.Context, userID int64, date time.Time) (bool, error) {
	var exists bool
	d := formatDate(date)
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM vacations WHERE user_id = ? AND start_date <= ? AND end_date >= ?)`,
		userID, d, d,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("query vacations: %w", err)
	}
	return exists, nil
}
