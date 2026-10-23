package parser

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var tz = time.FixedZone("UTC+7", 7*60*60)

func at(y int, m time.Month, d, h int) time.Time {
	return time.Date(y, m, d, h, 0, 0, 0, tz)
}

func day(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, tz)
}

func mustParse(t *testing.T, text, defaultIssue string, now time.Time) Report {
	t.Helper()
	r, err := Parse(text, defaultIssue, now)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return r
}

// lineErrors раскладывает ошибку Parse на номера строк
func lineErrors(t *testing.T, err error) []int {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		t.Fatalf("expected joined errors, got %v", err)
	}
	var lines []int
	for _, e := range joined.Unwrap() {
		var le *LineError
		if !errors.As(e, &le) {
			t.Fatalf("expected *LineError, got %T: %v", e, e)
		}
		lines = append(lines, le.Line)
	}
	return lines
}

func TestParseWeekSingleIssue(t *testing.T) {
	text := `12.10
Изучение аналитики, проработка архитектуры - 5 часов 30 минут

Груминг 2 - 2 часа 30 минут

13.10
Ревью контрактов - 2 часа

XYZ-2308: Правка путей proto модулей - 6 часов - реализация

14.10
XYZ-2304: Устранение старой реализации - реализация - 7 часов

XYZ-2308: Правка путей proto модулей - 1 час - реализация

15.10
XYZ-2304: Устранение старой реализации - реализация - 7 часов
XYZ-2308: Правка путей proto модулей - 1 час - реализация

16.10
XYZ-2287: Справочник типов карт - реализация - 8 часов`

	r := mustParse(t, text, "ABC-1", at(2026, 10, 20, 12))

	if len(r.Worklogs) != 5 {
		t.Fatalf("got %d worklogs, want 5", len(r.Worklogs))
	}
	for i, w := range r.Worklogs {
		if w.IssueKey != "ABC-1" {
			t.Errorf("worklog %d issue = %s, want ABC-1", i, w.IssueKey)
		}
		if w.Duration() != 8*time.Hour {
			t.Errorf("worklog %d (%s) = %v, want 8h", i, w.Date.Format("02.01"), w.Duration())
		}
		if want := day(2026, 10, 12+i); !w.Date.Equal(want) {
			t.Errorf("worklog %d date = %v, want %v", i, w.Date, want)
		}
	}
	if r.Total() != 40*time.Hour {
		t.Errorf("total = %v, want 40h", r.Total())
	}

	wantComment := "Изучение аналитики, проработка архитектуры - 5 часов 30 минут\n\nГруминг 2 - 2 часа 30 минут"
	if got := r.Worklogs[0].Comment(); got != wantComment {
		t.Errorf("comment = %q, want %q", got, wantComment)
	}
}

func TestParseMultipleIssues(t *testing.T) {
	text := `ABC-1
19.10
XYZ-101: Ревью контрактов - 2 часа
XYZ-102: Правка путей - 4 часа

20.10
XYZ-103: Справочник - 8 часов

ABC-2
19.10
Онбординг - 2 часа`

	r := mustParse(t, text, "", at(2026, 10, 20, 18))

	if len(r.Worklogs) != 3 {
		t.Fatalf("got %d worklogs, want 3", len(r.Worklogs))
	}
	want := []struct {
		issue string
		date  time.Time
		dur   time.Duration
	}{
		{"ABC-1", day(2026, 10, 19), 6 * time.Hour},
		{"ABC-2", day(2026, 10, 19), 2 * time.Hour},
		{"ABC-1", day(2026, 10, 20), 8 * time.Hour},
	}
	for i, w := range want {
		got := r.Worklogs[i]
		if got.IssueKey != w.issue || !got.Date.Equal(w.date) || got.Duration() != w.dur {
			t.Errorf("worklog %d = %s %s %v, want %s %s %v", i,
				got.IssueKey, got.Date.Format("02.01"), got.Duration(), w.issue, w.date.Format("02.01"), w.dur)
		}
	}
	if got := r.DayTotal(day(2026, 10, 19)); got != 8*time.Hour {
		t.Errorf("day total 19.10 = %v, want 8h", got)
	}
	if got := len(r.Dates()); got != 2 {
		t.Errorf("dates = %d, want 2", got)
	}
}

func TestParseDurations(t *testing.T) {
	tests := []struct {
		line string
		want time.Duration
	}{
		{"Работа - 1 час", time.Hour},
		{"Работа - 2 часа", 2 * time.Hour},
		{"Работа - 6 часов", 6 * time.Hour},
		{"Работа - 30 минут", 30 * time.Minute},
		{"Работа - 1 минута", time.Minute},
		{"Работа - 5 часов 30 минут", 5*time.Hour + 30*time.Minute},
		{"Работа - 2ч", 2 * time.Hour},
		{"Работа - 2 ч 15 мин", 2*time.Hour + 15*time.Minute},
		{"Работа - 2ч30мин", 2*time.Hour + 30*time.Minute},
		{"Работа - 1,5 часа", 90 * time.Minute},
		{"Работа - 1.5 ЧАСА", 90 * time.Minute},
		{"ABC-123 чинил баг - 3 часа", 3 * time.Hour},
		{"ABC-12 часовой пояс - 1 час", time.Hour},
		{"Созвон 2 раза - 45 минут", 45 * time.Minute},
		{"Ревью -2 часа 30 минут", 2*time.Hour + 30*time.Minute},
		{"Ревью:2 часа 30 минут", 2*time.Hour + 30*time.Minute},
		{"До 5 минимальных - 1 час", time.Hour},
		{"Задача про 12 часовой пояс - 1 час", time.Hour},
		{"Созвон в 10:30 - 30 минут", 30 * time.Minute},
		{"Работа - 15 минуту", 15 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.line, func(t *testing.T) {
			got, err := parseDuration(tt.line)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseDurationErrors(t *testing.T) {
	for _, line := range []string{
		"Работа без времени",
		"Работа - 2 часа и ещё 3 часа",
		"Работа - 10 минут 20 минут",
		"Работа - 0 часов",
		"ABC-123 чинил",
		"Работа - 9007199254741052 минут",
		"Работа - 25 часов",
		"Работа - 5 минимальных",
	} {
		t.Run(line, func(t *testing.T) {
			if d, err := parseDuration(line); err == nil {
				t.Fatalf("expected error, got %v", d)
			}
		})
	}
}

func TestParseYearBoundary(t *testing.T) {
	t.Run("декабрьская неделя отправлена в январе", func(t *testing.T) {
		r := mustParse(t, "28.12\nРабота - 8 часов\n30.12\nРабота - 7 часов", "ABC-1", at(2027, 1, 11, 10))
		if got := r.Worklogs[0].Date; !got.Equal(day(2026, 12, 28)) {
			t.Fatalf("date = %v, want 2026-12-28", got)
		}
	})

	t.Run("опечатка в будущую дату на стыке", func(t *testing.T) {
		_, err := Parse("02.01\nРабота - 8 часов", "ABC-1", at(2026, 12, 30, 10))
		if lines := lineErrors(t, err); len(lines) != 1 || lines[0] != 1 {
			t.Fatalf("error lines = %v, want [1]", lines)
		}
	})

	t.Run("1 января по локальному времени, в UTC ещё 31 декабря", func(t *testing.T) {
		now := at(2027, 1, 1, 1)
		if now.UTC().Day() != 31 {
			t.Fatal("test setup: expected UTC to still be Dec 31")
		}
		r := mustParse(t, "01.01\nДежурство - 1 час", "ABC-1", now)
		if got := r.Worklogs[0].Date; !got.Equal(day(2027, 1, 1)) {
			t.Fatalf("date = %v, want 2027-01-01", got)
		}
	})

	t.Run("29.02 в невисокосный год", func(t *testing.T) {
		_, err := Parse("29.02\nРабота - 8 часов", "ABC-1", at(2027, 3, 2, 10))
		if lines := lineErrors(t, err); len(lines) != 1 {
			t.Fatalf("error lines = %v, want one error", lines)
		}
	})

	t.Run("29.02 в високосный год", func(t *testing.T) {
		r := mustParse(t, "29.02\nРабота - 8 часов", "ABC-1", at(2028, 3, 2, 10))
		if got := r.Worklogs[0].Date; !got.Equal(day(2028, 2, 29)) {
			t.Fatalf("date = %v, want 2028-02-29", got)
		}
	})
}

func TestParseErrors(t *testing.T) {
	now := at(2026, 10, 20, 12)
	tests := []struct {
		name      string
		text      string
		wantLines []int
	}{
		{
			name:      "запись до даты",
			text:      "Работа - 2 часа\n19.10\nРабота - 6 часов",
			wantLines: []int{1},
		},
		{
			name:      "запись без длительности",
			text:      "19.10\nРабота",
			wantLines: []int{2},
		},
		{
			name:      "несуществующая дата, записи под ней молча пропускаются",
			text:      "31.02\nРабота - 2 часа\nЕщё - 1 час",
			wantLines: []int{1},
		},
		{
			name:      "будущая дата",
			text:      "21.10\nРабота - 2 часа",
			wantLines: []int{1},
		},
		{
			name:      "повтор даты в одной задаче",
			text:      "19.10\nРабота - 2 часа\n19.10\nЕщё - 1 час",
			wantLines: []int{3},
		},
		{
			name:      "после смены задачи нужна дата",
			text:      "19.10\nРабота - 2 часа\nABC-2\nЕщё - 1 час",
			wantLines: []int{4},
		},
		{
			name:      "больше суток за день по всем задачам",
			text:      "ABC-1\n19.10\nРабота - 20 часов\nABC-2\n19.10\nЕщё - 5 часов",
			wantLines: []int{3},
		},
		{
			name:      "все ошибки сразу и по порядку строк",
			text:      "Работа - 1 час\n19.10\nБез времени\n22.10\nРабота - 1 час",
			wantLines: []int{1, 3, 4},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(tt.text, "ABC-1", now)
			got := lineErrors(t, err)
			if len(got) != len(tt.wantLines) {
				t.Fatalf("error lines = %v, want %v (%v)", got, tt.wantLines, err)
			}
			for i := range got {
				if got[i] != tt.wantLines[i] {
					t.Fatalf("error lines = %v, want %v (%v)", got, tt.wantLines, err)
				}
			}
		})
	}
}

func TestParseEmptyAndNoIssue(t *testing.T) {
	now := at(2026, 10, 20, 12)
	if _, err := Parse("\n  \n", "ABC-1", now); err == nil {
		t.Fatal("expected error for empty post")
	}
	_, err := Parse("19.10\nРабота - 1 час", "", now)
	if lines := lineErrors(t, err); len(lines) != 1 || lines[0] != 2 {
		t.Fatalf("error lines = %v, want [2]", lines)
	}
}

func TestParseWhitespace(t *testing.T) {
	text := "\u00a019.10\u00a0\r\n  Работа\u00a0-\u00a02\u00a0часа  \r\n"
	r := mustParse(t, text, "ABC-1", at(2026, 10, 20, 12))
	if got := r.Worklogs[0].Entries[0].Text; got != "Работа - 2 часа" {
		t.Fatalf("entry text = %q", got)
	}
	if strings.ContainsAny(r.Worklogs[0].Comment(), "\r\u00a0") {
		t.Fatal("comment still contains CR or NBSP")
	}
}

func TestLooksLikePost(t *testing.T) {
	for text, want := range map[string]bool{
		"19.10\nРабота - 2 часа":         true,
		"\n\n  ABC-1\n19.10\nРабота":     true,
		"Купить молоко":                  false,
		"Заметка про 19.10\n19.10":       false,
		"":                               false,
		"\u00a019.10\u00a0\nРабота - 1ч": true,
	} {
		if got := LooksLikePost(text); got != want {
			t.Errorf("LooksLikePost(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		8 * time.Hour:                "8ч",
		7*time.Hour + 30*time.Minute: "7ч 30м",
		45 * time.Minute:             "45м",
		0:                            "0м",
	} {
		if got := FormatDuration(d); got != want {
			t.Errorf("FormatDuration(%v) = %q, want %q", d, got, want)
		}
	}
}
