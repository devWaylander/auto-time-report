package bot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"

	"github.com/devWaylander/auto-time-report/internal/calendar"
	"github.com/devWaylander/auto-time-report/internal/corp"
	"github.com/devWaylander/auto-time-report/internal/jira"
	"github.com/devWaylander/auto-time-report/internal/parser"
	"github.com/devWaylander/auto-time-report/internal/store"
	"github.com/devWaylander/auto-time-report/internal/store/storetest"
)

var tz = time.FixedZone("UTC+7", 7*60*60)

// fixedClock - "сейчас" для тестов: пятница 23.10.2026, полдень
func fixedClock() time.Time {
	return time.Date(2026, 10, 23, 12, 0, 0, 0, tz)
}

func day(m time.Month, d int) time.Time {
	return time.Date(2026, m, d, 0, 0, 0, 0, tz)
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

// newTestBot - бот без Telegram: хватает для логики поверх базы и календаря
func newTestBot(t *testing.T, cal fakeCalendar) (*Bot, *store.Store) {
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
	return &Bot{
		clock:  fixedClock,
		cfg:    Config{Token: "123:secret-token", OwnerID: 42, ChannelID: -100, DefaultIssue: "ABC-1", Location: tz},
		store:  st,
		cal:    cal,
		userID: u.ID,
	}, st
}

func TestSubmitDataRoundTrip(t *testing.T) {
	markup := previewKeyboard(previewKeys{Submit: true, PostID: 17, Hash: hashText("post"), Pending: 2, Filled: true}).(models.InlineKeyboardMarkup)
	for i, approve := range []bool{false, true} {
		data := markup.InlineKeyboard[0][i].CallbackData
		if len(data) > 64 {
			t.Fatalf("callback data %q exceeds Telegram's 64 bytes", data)
		}
		postID, hash, gotApprove, err := parseSubmitData(data)
		if err != nil {
			t.Fatal(err)
		}
		if postID != 17 || hash != hashText("post") || gotApprove != approve {
			t.Fatalf("parsed %d %s %v from %q", postID, hash, gotApprove, data)
		}
	}
	for _, bad := range []string{"s:", "s:abc:hash:1", "s:1:hash:2", "s:1:hash"} {
		if _, _, _, err := parseSubmitData(bad); err == nil {
			t.Errorf("parseSubmitData(%q) accepted", bad)
		}
	}
}

func TestPreviewKeyboard(t *testing.T) {
	data := func(m models.ReplyMarkup) [][]string {
		var rows [][]string
		for _, row := range m.(models.InlineKeyboardMarkup).InlineKeyboard {
			var r []string
			for _, btn := range row {
				if btn.URL != "" {
					r = append(r, "url")
					continue
				}
				r = append(r, btn.CallbackData)
			}
			rows = append(rows, r)
		}
		return rows
	}
	for _, tc := range []struct {
		name                string
		submit              bool
		pending, unapproved int
		filled              bool
		url                 string
		want                string
	}{
		{"status, nothing to do", false, 0, 0, true, "u", "[[v:status url]]"},
		{"status, no post link", false, 0, 0, true, "", "[[v:status]]"},
		{"status, something to send", false, 2, 0, true, "u", "[[v:submit] [v:status url]]"},
		{"status, something to send, week not filled", false, 2, 0, false, "u", "[[v:submit] [v:status url]]"},
		{"status, only approval", false, 0, 1, true, "u", "[[v:submit] [v:status url]]"},
		{"status, only approval, week not filled", false, 0, 1, false, "u", "[[v:status url]]"},
		{"submit, nothing to do", true, 0, 0, true, "u", "[[v:submit url x]]"},
		{"submit, only approval", true, 0, 1, true, "u", "[[s:1:h:1] [v:submit url x]]"},
		{"submit, only approval, week not filled", true, 0, 1, false, "u", "[[v:submit url x]]"},
		{"submit, worklogs", true, 2, 0, true, "u", "[[s:1:h:0 s:1:h:1] [v:submit url x]]"},
		{"submit, worklogs, week not filled", true, 2, 1, false, "", "[[s:1:h:0] [v:submit x]]"},
	} {
		keys := previewKeys{Submit: tc.submit, PostID: 1, Hash: "h", PostURL: tc.url, Pending: tc.pending, Unapproved: tc.unapproved, Filled: tc.filled}
		if got := fmt.Sprint(data(previewKeyboard(keys))); got != tc.want {
			t.Errorf("%s: keyboard = %s, want %s", tc.name, got, tc.want)
		}
	}
	if got := fmt.Sprint(data(statusKeyboard("u"))); got != "[[v:new url]]" {
		t.Errorf("status keyboard = %s", got)
	}
	if got := fmt.Sprint(data(statusKeyboard(""))); got != "[[v:new]]" {
		t.Errorf("status keyboard without post = %s", got)
	}
}

func TestPreviewInputAndRender(t *testing.T) {
	b, st := newTestBot(t, fakeCalendar{"2026-10-23": calendar.Short})
	ctx := context.Background()

	// Понедельник и вторник, во вторник две задачи; понедельник уже записан
	post := "19.10\nРабота - 8ч\n\n20.10\nДело <b>важное</b> - 6ч\n\nABC-2\n20.10\nСозвон - 2ч"
	saved, _, err := st.SaveWeekPost(ctx, store.WeekPost{UserID: b.userID, ChatID: -1001234567890, MessageID: 1, Text: post, PostedAt: day(10, 19)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveSubmission(ctx, store.Submission{
		UserID: b.userID, WeekPostID: saved.ID, IssueKey: "ABC-1", WorkDate: day(10, 19),
		Duration: 8 * time.Hour, Comment: "x", WorklogID: 1, PeriodID: 228,
	}); err != nil {
		t.Fatal(err)
	}

	w, err := b.loadWeek(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(w.pendingWorklogs()); got != 2 {
		t.Fatalf("pending worklogs = %d, want 2", got)
	}
	if len(w.unapproved) != 1 || w.unapproved[0] != 228 {
		t.Fatalf("unapproved = %v, want [228]", w.unapproved)
	}

	in := b.previewInput(ctx, w)
	// Пн-Пт рабочие, пятница сокращённая: 8*4 + 7
	if in.Norm != 39*time.Hour || in.Total != 16*time.Hour {
		t.Fatalf("norm %v total %v", in.Norm, in.Total)
	}
	if len(in.Days) != 5 {
		t.Fatalf("days = %d, want 5 working days", len(in.Days))
	}
	if in.Pending != 2 || in.Unapproved != 1 || !in.Unfilled {
		t.Fatalf("pending %d unapproved %d unfilled %v", in.Pending, in.Unapproved, in.Unfilled)
	}

	text := renderPreview(in)
	if n := strings.Count(text, "сегодня"); n != 1 {
		t.Errorf("today is marked %d times:\n%s", n, text)
	}
	for _, want := range []string{
		"<b>Неделя 19.10-23.10.2026</b>\n\n",
		"✅ Пн 19.10 - 8ч\n",
		"⏳ Вт 20.10 - 8ч\n",
		"▫️ Ср 21.10 -\n",
		"ABC-1 6ч, ABC-2 2ч",
		"▫️ Пт 23.10 - <i>(сокращённый)</i> · сегодня",
		"<b>Итого: 16ч</b> из 39ч по норме (не хватает 23ч)",
		"К записи в Jira: 2",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("preview has no %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "<b>важное</b>") {
		t.Error("post text must not leak into the preview as HTML")
	}
}

func TestPreviewApprovalWaitsForFilledWeek(t *testing.T) {
	b, st := newTestBot(t, nil)
	ctx := context.Background()

	// Сегодня пятница 23.10: Пн-Чт записаны, пятница пустая
	post := "19.10\nРабота - 8ч\n\n20.10\nРабота - 8ч\n\n21.10\nРабота - 8ч\n\n22.10\nРабота - 8ч\n\n23.10"
	saved, _, err := st.SaveWeekPost(ctx, store.WeekPost{UserID: b.userID, ChatID: -100, MessageID: 1, Text: post, PostedAt: day(10, 19)})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 4 {
		if _, err := st.SaveSubmission(ctx, store.Submission{
			UserID: b.userID, WeekPostID: saved.ID, IssueKey: "ABC-1", WorkDate: day(10, 19+i),
			Duration: 8 * time.Hour, Comment: "x", WorklogID: int64(i + 1), PeriodID: 228,
		}); err != nil {
			t.Fatal(err)
		}
	}

	status, markup := b.buildPreview(ctx, false)
	if !strings.Contains(status, "✅ В Jira есть всё из поста") || strings.Contains(status, "проверку") {
		t.Errorf("unfilled week must not be offered for approval:\n%s", status)
	}
	if got := len(markup.(models.InlineKeyboardMarkup).InlineKeyboard[0]); got != 1 {
		t.Errorf("status keyboard has %d buttons, want only refresh", got)
	}
	submit, markup := b.buildPreview(ctx, true)
	if !strings.Contains(submit, "На проверку после заполнения всех рабочих дней") {
		t.Errorf("submit preview does not explain the missing approval:\n%s", submit)
	}
	if rows := markup.(models.InlineKeyboardMarkup).InlineKeyboard; len(rows) != 1 {
		t.Errorf("submit keyboard = %v, want only refresh and hide", rows)
	}

	// Пятница в отпуске - неделя заполнена
	if _, err := st.AddVacation(ctx, b.userID, day(10, 23), day(10, 23)); err != nil {
		t.Fatal(err)
	}
	status, markup = b.buildPreview(ctx, false)
	if !strings.Contains(status, "Неделя заполнена, можно отправить на проверку") || strings.Contains(status, "Нет записей") {
		t.Errorf("vacation day must count as filled:\n%s", status)
	}
	if rows := markup.(models.InlineKeyboardMarkup).InlineKeyboard; len(rows) != 2 || rows[0][0].CallbackData != viewSubmit {
		t.Errorf("status keyboard = %v, want submit above refresh", rows)
	}
}

func TestRenderShortfall(t *testing.T) {
	text := renderPreview(previewInput{
		Days: []dayLine{
			{Date: day(10, 19), Kind: calendar.Working, Total: 8 * time.Hour, Worklogs: 1},
			{Date: day(10, 20), Kind: calendar.Working, Total: 6 * time.Hour, Worklogs: 1},
			{Date: day(10, 21), Kind: calendar.Working, Total: 2 * time.Hour, Worklogs: 1, Vacation: true},
			{Date: day(10, 23), Kind: calendar.Short, Total: 6*time.Hour + 30*time.Minute, Worklogs: 1},
			{Date: day(10, 24), Kind: calendar.Off, Total: 2 * time.Hour, Worklogs: 1},
		},
		Pending: 1,
	})
	for _, want := range []string{
		"⏳ Пн 19.10 - 8ч\n",
		"⏳ Вт 20.10 - 6ч из 8ч\n",
		"⏳ Ср 21.10 - 2ч\n",
		"⏳ Пт 23.10 - 6ч 30м из 7ч <i>(сокращённый)</i>",
		"⏳ Сб 24.10 - 2ч <i>(выходной)</i>",
		"⚠️ Недобор: 20.10 (2ч), 23.10 (30м)\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("preview has no %q:\n%s", want, text)
		}
	}
}

func TestChangedSubmitted(t *testing.T) {
	b, st := newTestBot(t, nil)
	ctx := context.Background()
	for _, s := range []store.Submission{
		{UserID: b.userID, IssueKey: "ABC-1", WorkDate: day(10, 19), Duration: 8 * time.Hour, Comment: "x", WorklogID: 1, PeriodID: 1},
		{UserID: b.userID, IssueKey: "ABC-1", WorkDate: day(10, 20), Duration: 8 * time.Hour, Comment: "x", WorklogID: 2, PeriodID: 1},
	} {
		if _, err := st.SaveSubmission(ctx, s); err != nil {
			t.Fatal(err)
		}
	}

	report, err := parseAt("19.10\nРабота - 7ч\n\n21.10\nРабота - 8ч", time.Date(2026, 10, 21, 12, 0, 0, 0, tz))
	if err != nil {
		t.Fatal(err)
	}
	warning, err := b.changedSubmitted(ctx, report)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(warning, "19.10 ABC-1: в Jira 8ч, в посте теперь 7ч") {
		t.Errorf("no warning about changed day:\n%s", warning)
	}
	// 20.10 записан в Jira, а из поста пропал
	if !strings.Contains(warning, "20.10 ABC-1: в Jira 8ч, а из поста запись пропала") {
		t.Errorf("no warning about removed day:\n%s", warning)
	}
}

func TestParseVacation(t *testing.T) {
	tests := []struct {
		in         string
		today      time.Time
		start, end string
		wantErr    bool
	}{
		{in: "27.10-31.10", today: day(10, 20), start: "2026-10-27", end: "2026-10-31"},
		{in: "28.12 - 05.01", today: day(12, 1), start: "2026-12-28", end: "2027-01-05"},
		{in: "05.01-10.01", today: day(12, 20), start: "2027-01-05", end: "2027-01-10"},
		{in: "19.10-21.10", today: day(10, 20), start: "2026-10-19", end: "2026-10-21"},
		{in: "31.02-05.03", today: day(1, 10), wantErr: true},
		{in: "01.01-01.06", today: day(1, 10), wantErr: true},
		{in: "завтра", today: day(1, 10), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			start, end, err := parseVacation(tt.in, tt.today)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %v-%v", start, end)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if start.Format("2006-01-02") != tt.start || end.Format("2006-01-02") != tt.end {
				t.Fatalf("got %s-%s, want %s-%s", start.Format("2006-01-02"), end.Format("2006-01-02"), tt.start, tt.end)
			}
		})
	}
}

func TestDescribeError(t *testing.T) {
	if got := describeError(corp.ErrBadPassphrase); !strings.Contains(got, "не подошла") {
		t.Errorf("bad passphrase: %s", got)
	}
	auth := fmt.Errorf("check: %w", jira.ErrAuth)
	if !isAuthError(auth) || !strings.Contains(describeError(auth), "/unlock") {
		t.Errorf("auth error: %s", describeError(auth))
	}
	if !isAuthError(corp.ErrVPNAuth) {
		t.Error("vpn auth error is not recognized")
	}
	if got := describeError(errors.New("<script>")); strings.Contains(got, "<script>") {
		t.Errorf("error text is not escaped: %s", got)
	}
}

func TestRenderOutcome(t *testing.T) {
	o := corp.Outcome{
		Worklogs: []corp.WorklogResult{
			{Job: corp.WorklogJob{IssueKey: "ABC-1", Date: day(10, 19), Duration: 8 * time.Hour}, Status: corp.StatusCreated},
			{Job: corp.WorklogJob{IssueKey: "ABC-1", Date: day(10, 20), Duration: 8 * time.Hour}, Status: corp.StatusFailed},
			{Job: corp.WorklogJob{IssueKey: "ABC-1", Date: day(10, 21), Duration: 8 * time.Hour}, Status: corp.StatusSkipped},
		},
		Approved:    []int64{228},
		CalendarURL: "https://jira.example.com/secure/WorklogPROWorklogCalendar!default.jspa",
	}
	text := renderOutcome(o, errors.New("boom"))
	for _, want := range []string{"Остановлено", "19.10 ABC-1 8ч - записано", "20.10 ABC-1 8ч - ошибка", "21.10 ABC-1 8ч - очередь не дошла", "Период 228"} {
		if !strings.Contains(text, want) {
			t.Errorf("outcome has no %q:\n%s", want, text)
		}
	}
}

func TestRedact(t *testing.T) {
	b, _ := newTestBot(t, nil)
	got := b.redact(`Post "https://api.telegram.org/bot123:secret-token/getUpdates": EOF`)
	if strings.Contains(got, "secret-token") {
		t.Fatalf("token leaked: %s", got)
	}
}

func TestFormatParseErrorsEscapes(t *testing.T) {
	got := formatParseErrors(errors.New("строка 1: <b>x</b>\nстрока 2: y"))
	if strings.Contains(got, "<b>") || strings.Count(got, "• ") != 2 {
		t.Fatalf("formatParseErrors = %q", got)
	}
}

func TestWeekStart(t *testing.T) {
	for d, want := range map[time.Time]time.Time{
		day(10, 19): day(10, 19),
		day(10, 23): day(10, 19),
		day(10, 25): day(10, 19),
		day(10, 26): day(10, 26),
	} {
		if got := weekStart(d); !got.Equal(want) {
			t.Errorf("weekStart(%s) = %s, want %s", d.Format("02.01"), got.Format("02.01"), want.Format("02.01"))
		}
	}
}

func TestRefusal(t *testing.T) {
	b, _ := newTestBot(t, nil)
	if r := b.refusalLocked(); r != "" {
		t.Fatalf("unexpected refusal: %s", r)
	}
	for range maxAttemptsPerHour {
		b.attempts = append(b.attempts, b.clock())
	}
	if b.refusalLocked() == "" {
		t.Fatal("no refusal after too many attempts")
	}
	b.attempts = []time.Time{b.clock().Add(-2 * time.Hour)}
	if r := b.refusalLocked(); r != "" || len(b.attempts) != 0 {
		t.Fatalf("old attempts not pruned: %q %v", r, b.attempts)
	}
	b.locked = true
	if !strings.Contains(b.refusalLocked(), "/unlock") {
		t.Fatal("no refusal while locked")
	}
}

func parseAt(text string, now time.Time) (parser.Report, error) {
	return parser.Parse(text, "ABC-1", now)
}

func TestPostURL(t *testing.T) {
	for _, tc := range []struct {
		chat, msg int64
		want      string
	}{
		{-1001234567890, 42, "https://t.me/c/1234567890/42"},
		{-1234567, 42, ""}, // обычная группа, не канал
		{123, 42, ""},      // личка
		{-100, 42, ""},
		{-1001234567890, 0, ""},
	} {
		if got := postURL(tc.chat, tc.msg); got != tc.want {
			t.Errorf("postURL(%d, %d) = %q, want %q", tc.chat, tc.msg, got, tc.want)
		}
	}
}

// Под ошибками в посте - кнопка на сам пост, ссылка в тексте не нужна
func TestPreviewErrorsHavePostButton(t *testing.T) {
	b, st := newTestBot(t, nil)
	ctx := context.Background()
	if _, _, err := st.SaveWeekPost(ctx, store.WeekPost{UserID: b.userID, ChatID: -1001234567890, MessageID: 7, Text: "19.10\nРабота", PostedAt: day(10, 19)}); err != nil {
		t.Fatal(err)
	}
	text, markup := b.buildPreview(ctx, false)
	if !strings.HasPrefix(text, "⚠️ В посте недели ошибки, предпросмотр не собран:\n") || strings.Contains(text, "<a ") {
		t.Errorf("errors text:\n%s", text)
	}
	rows := markup.(models.InlineKeyboardMarkup).InlineKeyboard
	if len(rows) != 1 || len(rows[0]) != 2 || rows[0][1].URL != "https://t.me/c/1234567890/7" {
		t.Errorf("errors keyboard = %+v, want refresh and post", rows)
	}
}
