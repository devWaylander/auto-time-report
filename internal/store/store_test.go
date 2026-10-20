package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// newTestStore создаёт базу во временной папке и накатывает на неё up-части миграций,
// повторяя то, что делает dbmate
func newTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")

	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := raw.Close(); err != nil {
			t.Error(err)
		}
	}()

	if _, err := raw.Exec(`CREATE TABLE schema_migrations (version TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}

	files, err := filepath.Glob("../../db/migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no migrations found")
	}
	sort.Strings(files)

	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		up, _, ok := strings.Cut(string(body), "-- migrate:down")
		if !ok {
			t.Fatalf("%s: no migrate:down section", f)
		}
		if _, err := raw.Exec(up); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		version, _, _ := strings.Cut(filepath.Base(f), "_")
		if _, err := raw.Exec(`INSERT INTO schema_migrations (version) VALUES (?)`, version); err != nil {
			t.Fatal(err)
		}
	}

	s, err := Open(context.Background(), "sqlite:"+path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func date(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := time.Parse(dateLayout, s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestOpenRequiresMigrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.db")
	if _, err := Open(context.Background(), "sqlite:"+path); err == nil {
		t.Fatal("expected error for database without migrations")
	}
}

func TestPathFromURL(t *testing.T) {
	tests := []struct {
		url     string
		want    string
		wantErr bool
	}{
		{url: "sqlite:./data/bot.db", want: "./data/bot.db"},
		{url: "sqlite:data/bot.db", want: "data/bot.db"},
		{url: "sqlite:/data/bot.db", want: "/data/bot.db"},
		{url: "sqlite:///data/bot.db", want: "/data/bot.db"},
		{url: "sqlite3:/data/bot.db", want: "/data/bot.db"},
		{url: "postgres://localhost/db", wantErr: true},
		{url: "sqlite:", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			got, err := pathFromURL(tt.url)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEnsureUserCreatesDefaultSettings(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	u, err := s.EnsureUser(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.EnsureUser(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != u.ID {
		t.Fatalf("EnsureUser created a duplicate: %d != %d", again.ID, u.ID)
	}

	st, err := s.Settings(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := Settings{
		UserID: u.ID, DailyEnabled: true, DailyTime: "19:00",
		WeeklyEnabled: true, WeeklyWeekday: time.Friday, WeeklyTime: "16:00",
	}
	st.UpdatedAt = time.Time{}
	if st != want {
		t.Fatalf("default settings = %+v, want %+v", st, want)
	}

	if _, err := s.UserByTelegramID(ctx, 7); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UserByTelegramID(unknown) error = %v, want ErrNotFound", err)
	}
}

func TestUpdateSettings(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, err := s.EnsureUser(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}

	paused := time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)
	st := Settings{
		UserID: u.ID, DailyEnabled: false, DailyTime: "20:30",
		WeeklyEnabled: true, WeeklyWeekday: time.Thursday, WeeklyTime: "15:00",
		PausedUntil: paused,
	}
	if err := s.UpdateSettings(ctx, st); err != nil {
		t.Fatal(err)
	}

	got, err := s.Settings(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	got.UpdatedAt = time.Time{}
	if !got.PausedUntil.Equal(paused) {
		t.Fatalf("PausedUntil = %v, want %v", got.PausedUntil, paused)
	}
	got.PausedUntil, st.PausedUntil = time.Time{}, time.Time{}
	if got != st {
		t.Fatalf("settings = %+v, want %+v", got, st)
	}

	st.DailyTime = "25:00"
	if err := s.UpdateSettings(ctx, st); err == nil {
		t.Fatal("expected CHECK constraint error for invalid time")
	}
}

func TestVacations(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, err := s.EnsureUser(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.AddVacation(ctx, u.ID, date(t, "2026-10-27"), date(t, "2026-10-31")); err != nil {
		t.Fatal(err)
	}

	for d, want := range map[string]bool{
		"2026-10-26": false,
		"2026-10-27": true,
		"2026-10-29": true,
		"2026-10-31": true,
		"2026-11-01": false,
	} {
		got, err := s.OnVacation(ctx, u.ID, date(t, d))
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("OnVacation(%s) = %v, want %v", d, got, want)
		}
	}

	if err := s.CancelVacations(ctx, u.ID, date(t, "2026-10-28")); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.OnVacation(ctx, u.ID, date(t, "2026-10-29")); got {
		t.Fatal("vacation is still active after cancel")
	}

	if _, err := s.AddVacation(ctx, u.ID, date(t, "2026-11-05"), date(t, "2026-11-01")); err == nil {
		t.Fatal("expected CHECK constraint error for end before start")
	}
}

func TestWeekPostUpsert(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, err := s.EnsureUser(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.LatestWeekPost(ctx, u.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("LatestWeekPost(no posts) error = %v, want ErrNotFound", err)
	}

	posted := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	first, err := s.SaveWeekPost(ctx, WeekPost{
		UserID: u.ID, ChatID: -100, MessageID: 1, Text: "19.10\nA - 8 часов", PostedAt: posted,
	})
	if err != nil {
		t.Fatal(err)
	}

	edited := posted.Add(24 * time.Hour)
	second, err := s.SaveWeekPost(ctx, WeekPost{
		UserID: u.ID, ChatID: -100, MessageID: 1, Text: "19.10\nA - 8 часов\n\n20.10\nB - 8 часов",
		PostedAt: posted, EditedAt: edited,
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Fatalf("edit created a new post: %d != %d", second.ID, first.ID)
	}

	got, err := s.LatestWeekPost(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != second.Text || !got.EditedAt.Equal(edited) || !got.PostedAt.Equal(posted) {
		t.Fatalf("LatestWeekPost = %+v", got)
	}

	next, err := s.SaveWeekPost(ctx, WeekPost{
		UserID: u.ID, ChatID: -100, MessageID: 2, Text: "26.10\nC - 8 часов", PostedAt: posted.Add(7 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.LatestWeekPost(ctx, u.ID); got.ID != next.ID {
		t.Fatalf("LatestWeekPost returned post %d, want %d", got.ID, next.ID)
	}
}

func TestSubmissions(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, err := s.EnsureUser(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}

	sub := Submission{
		UserID: u.ID, IssueKey: "ABC-1", WorkDate: date(t, "2026-10-19"),
		Duration: 7*time.Hour + 30*time.Minute, Comment: "XYZ-1: ревью - 7 часов 30 минут",
		WorklogID: 1234567, PeriodID: 123,
	}
	saved, err := s.SaveSubmission(ctx, sub)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveSubmission(ctx, sub); err == nil {
		t.Fatal("expected UNIQUE error for the same issue and date")
	}

	other := sub
	other.IssueKey = "ABC-2"
	other.Duration = 30 * time.Minute
	if _, err := s.SaveSubmission(ctx, other); err != nil {
		t.Fatalf("same date, other issue: %v", err)
	}

	later := sub
	later.WorkDate = date(t, "2026-10-27")
	if _, err := s.SaveSubmission(ctx, later); err != nil {
		t.Fatal(err)
	}

	got, err := s.SubmissionsBetween(ctx, u.ID, date(t, "2026-10-19"), date(t, "2026-10-23"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d submissions, want 2", len(got))
	}
	first := got[0]
	if first.ID != saved.ID || first.Duration != sub.Duration || first.Comment != sub.Comment ||
		!first.WorkDate.Equal(sub.WorkDate) || first.WorklogID != sub.WorklogID || first.PeriodID != sub.PeriodID {
		t.Fatalf("submission = %+v, want %+v", first, saved)
	}
}

func TestPeriodApprovals(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, err := s.EnsureUser(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}

	if ok, _ := s.PeriodRequested(ctx, u.ID, 123); ok {
		t.Fatal("period requested before marking")
	}
	for range 2 {
		if err := s.MarkPeriodRequested(ctx, u.ID, 123); err != nil {
			t.Fatal(err)
		}
	}
	if ok, _ := s.PeriodRequested(ctx, u.ID, 123); !ok {
		t.Fatal("period not requested after marking")
	}
}

func TestClaimNotification(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, err := s.EnsureUser(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	d := date(t, "2026-10-20")

	if ok, err := s.ClaimNotification(ctx, u.ID, NotificationDaily, d); err != nil || !ok {
		t.Fatalf("first claim = %v, %v; want true", ok, err)
	}
	if ok, err := s.ClaimNotification(ctx, u.ID, NotificationDaily, d); err != nil || ok {
		t.Fatalf("second claim = %v, %v; want false", ok, err)
	}
	if ok, err := s.ClaimNotification(ctx, u.ID, NotificationWeekly, d); err != nil || !ok {
		t.Fatalf("other kind claim = %v, %v; want true", ok, err)
	}
	if _, err := s.ClaimNotification(ctx, u.ID, "monthly", d); err == nil {
		t.Fatal("expected CHECK constraint error for unknown kind")
	}
}

func TestDeleteUserCascades(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, err := s.EnsureUser(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNotification(ctx, u.ID, NotificationDaily, date(t, "2026-10-20")); err != nil {
		t.Fatal(err)
	}

	if _, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Settings(ctx, u.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("settings survived user deletion: %v", err)
	}
}
