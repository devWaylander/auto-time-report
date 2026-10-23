// Package notify решает, когда напоминать: вечером о пропущенном дне и в выбранный день недели превью
// с кнопкой отправки. Проверки идут только по локальной базе, в Jira не ходим
package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/devWaylander/auto-time-report/internal/calendar"
	"github.com/devWaylander/auto-time-report/internal/parser"
	"github.com/devWaylander/auto-time-report/internal/store"
)

// Output отправляет напоминания пользователю, реализуется ботом
type Output interface {
	// DailyReminder - за сегодня в посте нет записей
	DailyReminder(ctx context.Context, date time.Time) error
	// WeeklyPreview - превью недели с кнопками отправки
	WeeklyPreview(ctx context.Context) error
}

type Calendar interface {
	Day(ctx context.Context, date time.Time) (calendar.DayKind, error)
}

type Notifier struct {
	store        *store.Store
	cal          Calendar
	out          Output
	loc          *time.Location
	userID       int64
	defaultIssue string
}

func New(st *store.Store, cal Calendar, out Output, loc *time.Location, userID int64, defaultIssue string) *Notifier {
	return &Notifier{store: st, cal: cal, out: out, loc: loc, userID: userID, defaultIssue: defaultIssue}
}

// Run проверяет раз в минуту, пока не отменят контекст
func (n *Notifier) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		if err := n.Tick(ctx, time.Now()); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("reminders", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Tick - одна проверка на момент now; каждое напоминание уходит не больше раза в день, даже после рестарта
func (n *Notifier) Tick(ctx context.Context, now time.Time) error {
	now = now.In(n.loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, n.loc)

	st, err := n.store.Settings(ctx, n.userID)
	if err != nil {
		return fmt.Errorf("load settings: %w", err)
	}
	if now.Before(st.PausedUntil) {
		return nil
	}
	onVacation, err := n.store.OnVacation(ctx, n.userID, today)
	if err != nil {
		return err
	}
	if onVacation {
		return nil
	}

	kind, err := n.cal.Day(ctx, today)
	if err != nil {
		// Календарь вернул пятидневку по умолчанию, напоминать всё равно можно
		slog.Warn("production calendar unavailable, using Mon-Fri", "error", err)
	}
	if kind == calendar.Off {
		return nil
	}

	var errs []error
	if st.DailyEnabled && reached(now, today, st.DailyTime) {
		errs = append(errs, n.daily(ctx, now, today))
	}
	if st.WeeklyEnabled && today.Weekday() == st.WeeklyWeekday && reached(now, today, st.WeeklyTime) {
		errs = append(errs, n.weekly(ctx, today))
	}
	return errors.Join(errs...)
}

func (n *Notifier) daily(ctx context.Context, now, today time.Time) error {
	has, err := n.postHasDate(ctx, now, today)
	if err != nil || has {
		return err
	}
	claimed, err := n.store.ClaimNotification(ctx, n.userID, store.NotificationDaily, today)
	if err != nil || !claimed {
		return err
	}
	return n.out.DailyReminder(ctx, today)
}

func (n *Notifier) weekly(ctx context.Context, today time.Time) error {
	claimed, err := n.store.ClaimNotification(ctx, n.userID, store.NotificationWeekly, today)
	if err != nil || !claimed {
		return err
	}
	return n.out.WeeklyPreview(ctx)
}

// postHasDate - есть ли в последнем посте записи за дату. Пост с ошибками считается без записей:
// об ошибках бот уже написал при правке, а напоминание лишним не будет
func (n *Notifier) postHasDate(ctx context.Context, now, date time.Time) (bool, error) {
	post, err := n.store.LatestWeekPost(ctx, n.userID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	report, err := parser.Parse(post.Text, n.defaultIssue, now)
	if err != nil {
		return false, nil
	}
	return report.DayTotal(date) > 0, nil
}

// reached - наступило ли время ЧЧ:ММ в этот день
func reached(now, today time.Time, hhmm string) bool {
	t, err := time.Parse("15:04", hhmm)
	if err != nil {
		return false
	}
	at := today.Add(time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute)
	return !now.Before(at)
}
