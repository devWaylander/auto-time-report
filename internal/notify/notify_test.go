package notify

import (
	"context"
	"testing"
	"time"

	"github.com/devWaylander/auto-time-report/internal/calendar"
	"github.com/devWaylander/auto-time-report/internal/store"
	"github.com/devWaylander/auto-time-report/internal/store/storetest"
)

var tz = time.FixedZone("UTC+7", 7*60*60)

type fakeOutput struct {
	daily  []time.Time
	weekly int
}

func (f *fakeOutput) DailyReminder(_ context.Context, date time.Time) error {
	f.daily = append(f.daily, date)
	return nil
}

func (f *fakeOutput) WeeklyPreview(context.Context) error {
	f.weekly++
	return nil
}

type fakeCalendar map[string]calendar.DayKind

func (f fakeCalendar) Day(_ context.Context, date time.Time) (calendar.DayKind, error) {
	if k, ok := f[date.Format("2006-01-02")]; ok {
		return k, nil
	}
	if wd := date.Weekday(); wd == time.Saturday || wd == time.Sunday {
		return calendar.Off, nil
	}
	return calendar.Working, nil
}

func setup(t *testing.T, cal fakeCalendar) (*Notifier, *fakeOutput, *store.Store, store.User) {
	t.Helper()
	st, err := store.Open(context.Background(), storetest.NewDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	u, err := st.EnsureUser(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	out := &fakeOutput{}
	return New(st, cal, out, tz, u.ID, "ABC-1"), out, st, u
}

func at(d, h, m int) time.Time {
	return time.Date(2026, 10, d, h, m, 0, 0, tz)
}

func savePost(t *testing.T, st *store.Store, u store.User, text string) {
	t.Helper()
	_, err := st.SaveWeekPost(context.Background(), store.WeekPost{
		UserID: u.ID, ChatID: -100, MessageID: 1, Text: text, PostedAt: at(19, 9, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestDailyReminder(t *testing.T) {
	n, out, st, u := setup(t, nil)
	ctx := context.Background()
	savePost(t, st, u, "19.10\nРабота - 8 часов")

	// Вторник 20.10: до 19:00 молчим
	if err := n.Tick(ctx, at(20, 18, 59)); err != nil {
		t.Fatal(err)
	}
	if len(out.daily) != 0 {
		t.Fatal("reminded before daily time")
	}

	// После 19:00 напоминаем один раз, даже если проверок много
	for _, m := range []int{0, 1, 30} {
		if err := n.Tick(ctx, at(20, 19, m)); err != nil {
			t.Fatal(err)
		}
	}
	if len(out.daily) != 1 || !out.daily[0].Equal(time.Date(2026, 10, 20, 0, 0, 0, 0, tz)) {
		t.Fatalf("daily reminders = %v, want one for 20.10", out.daily)
	}

	// День уже записан - напоминания нет
	n2, out2, st2, u2 := setup(t, nil)
	savePost(t, st2, u2, "20.10\nРабота - 8 часов")
	if err := n2.Tick(ctx, at(20, 20, 0)); err != nil {
		t.Fatal(err)
	}
	if len(out2.daily) != 0 {
		t.Fatal("reminded although today is in the post")
	}
}

func TestNoRemindersOnDaysOff(t *testing.T) {
	n, out, _, _ := setup(t, fakeCalendar{"2026-11-04": calendar.Off})
	ctx := context.Background()
	// Суббота и праздник в среду
	for _, now := range []time.Time{at(24, 20, 0), time.Date(2026, 11, 4, 20, 0, 0, 0, tz)} {
		if err := n.Tick(ctx, now); err != nil {
			t.Fatal(err)
		}
	}
	if len(out.daily) != 0 || out.weekly != 0 {
		t.Fatalf("reminders on days off: %+v", out)
	}
}

func TestWeeklyPreview(t *testing.T) {
	n, out, st, u := setup(t, nil)
	ctx := context.Background()
	savePost(t, st, u, "23.10\nРабота - 8 часов")

	// Пятница 23.10, по умолчанию превью в 16:00
	if err := n.Tick(ctx, at(23, 15, 59)); err != nil {
		t.Fatal(err)
	}
	if out.weekly != 0 {
		t.Fatal("weekly preview before time")
	}
	for range 3 {
		if err := n.Tick(ctx, at(23, 16, 5)); err != nil {
			t.Fatal(err)
		}
	}
	if out.weekly != 1 {
		t.Fatalf("weekly previews = %d, want 1", out.weekly)
	}

	// Не пятница - не шлём
	n2, out2, _, _ := setup(t, nil)
	if err := n2.Tick(ctx, at(22, 17, 0)); err != nil {
		t.Fatal(err)
	}
	if out2.weekly != 0 {
		t.Fatal("weekly preview on the wrong weekday")
	}
}

func TestPauseAndVacation(t *testing.T) {
	n, out, st, u := setup(t, nil)
	ctx := context.Background()

	s, err := st.Settings(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	s.PausedUntil = at(21, 0, 0)
	if err := st.UpdateSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	if err := n.Tick(ctx, at(20, 20, 0)); err != nil {
		t.Fatal(err)
	}
	if len(out.daily) != 0 {
		t.Fatal("reminded during pause")
	}

	if _, err := st.AddVacation(ctx, u.ID, at(21, 0, 0), at(22, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if err := n.Tick(ctx, at(21, 20, 0)); err != nil {
		t.Fatal(err)
	}
	if len(out.daily) != 0 {
		t.Fatal("reminded on vacation")
	}

	if err := n.Tick(ctx, at(23, 20, 0)); err != nil {
		t.Fatal(err)
	}
	if len(out.daily) != 1 {
		t.Fatalf("daily reminders after vacation = %d, want 1", len(out.daily))
	}
}

func TestDisabled(t *testing.T) {
	n, out, st, u := setup(t, nil)
	ctx := context.Background()
	s, err := st.Settings(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	s.DailyEnabled, s.WeeklyEnabled = false, false
	if err := st.UpdateSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	if err := n.Tick(ctx, at(23, 20, 0)); err != nil {
		t.Fatal(err)
	}
	if len(out.daily) != 0 || out.weekly != 0 {
		t.Fatalf("reminders while disabled: %+v", out)
	}
}
