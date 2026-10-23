package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"

	"github.com/devWaylander/auto-time-report/internal/corp"
	"github.com/devWaylander/auto-time-report/internal/store"
)

// apiCall - один запрос бота к Bot API
type apiCall struct {
	method string
	params map[string]string
}

// fakeTelegram записывает вызовы Bot API и отвечает успехом
type fakeTelegram struct {
	mu     sync.Mutex
	calls  []apiCall
	nextID int
}

func (f *fakeTelegram) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	params := map[string]string{}
	if err := r.ParseMultipartForm(1 << 20); err == nil {
		for k, v := range r.MultipartForm.Value {
			params[k] = v[0]
		}
	}

	f.mu.Lock()
	f.calls = append(f.calls, apiCall{method: method, params: params})
	f.nextID++
	id := f.nextID
	f.mu.Unlock()

	var result any = true
	switch method {
	case "getMe":
		result = map[string]any{"id": 1, "is_bot": true, "first_name": "bot", "username": "bot"}
	case "sendMessage", "editMessageText", "editMessageReplyMarkup":
		result = map[string]any{"message_id": 1000 + id, "date": 0, "chat": map[string]any{"id": 42, "type": "private"}}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
}

func (f *fakeTelegram) take() []apiCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	calls := f.calls
	f.calls = nil
	return calls
}

func (f *fakeTelegram) sent(calls []apiCall) []string {
	var texts []string
	for _, c := range calls {
		if c.method == "sendMessage" || c.method == "editMessageText" {
			texts = append(texts, c.params["text"])
		}
	}
	return texts
}

func countMethod(calls []apiCall, method string) int {
	n := 0
	for _, c := range calls {
		if c.method == method {
			n++
		}
	}
	return n
}

// fakeRunner вместо корпоративной части: запоминает фразу и задание, "записывает" всё
type fakeRunner struct {
	passphrase string
	jobs       []corp.Job
}

func (r *fakeRunner) Run(_ context.Context, passphrase []byte, job corp.Job, hooks corp.Hooks) (corp.Outcome, error) {
	r.passphrase = string(passphrase)
	r.jobs = append(r.jobs, job)
	out := corp.Outcome{}
	for i, w := range job.Worklogs {
		res := corp.WorklogResult{Job: w, Status: corp.StatusCreated, WorklogID: int64(5000 + i), PeriodID: 228}
		if err := hooks.Created(res); err != nil {
			return out, err
		}
		out.Worklogs = append(out.Worklogs, res)
	}
	return out, nil
}

func newTelegramBot(t *testing.T) (*Bot, *fakeTelegram, *fakeRunner, *store.Store) {
	t.Helper()
	tg := &fakeTelegram{}
	srv := httptest.NewServer(tg)
	t.Cleanup(srv.Close)

	base, st := newTestBot(t, nil)
	runner := &fakeRunner{}
	cfg := base.cfg
	cfg.ServerURL = srv.URL
	b, err := New(cfg, st, base.cal, runner, base.userID)
	if err != nil {
		t.Fatal(err)
	}
	b.clock = fixedClock
	tg.take()
	return b, tg, runner, st
}

var nextMessageID = 100

func channelPost(text string, posted time.Time) *models.Message {
	nextMessageID++
	return &models.Message{ID: nextMessageID, Date: int(posted.Unix()), Chat: models.Chat{ID: -100}, Text: text}
}

func edit(msg *models.Message, text string, at time.Time) *models.Message {
	e := *msg
	e.Text, e.EditDate = text, int(at.Unix())
	return &e
}

func dm(text string) *models.Message {
	nextMessageID++
	return &models.Message{ID: nextMessageID, From: &models.User{ID: 42}, Chat: models.Chat{ID: 42}, Text: text}
}

func latestText(t *testing.T, b *Bot) string {
	t.Helper()
	p, err := b.store.LatestWeekPost(context.Background(), b.userID)
	if err != nil {
		t.Fatal(err)
	}
	return p.Text
}

func TestChannelNotesDoNotReplaceWeekPost(t *testing.T) {
	b, tg, _, _ := newTelegramBot(t)
	ctx := context.Background()
	monday := day(10, 19)

	week := channelPost("19.10\nРабота - 8 часов", monday)
	b.onChannelPost(ctx, week, false)
	if latestText(t, b) != week.Text {
		t.Fatal("week post not accepted")
	}

	// Заметки: ключ отдельной строкой, дата посреди недели, обычный текст
	for _, note := range []string{
		"ABC-123\nспросить у аналитика про лимиты",
		"21.10\nСозвон с командой - 1 час",
		"Купить молоко",
	} {
		b.onChannelPost(ctx, channelPost(note, day(10, 21)), false)
	}
	if latestText(t, b) != week.Text {
		t.Fatalf("a note replaced the week post: %q", latestText(t, b))
	}
	if calls := tg.take(); len(calls) != 0 {
		t.Fatalf("notes produced bot messages: %+v", calls)
	}

	// Правка известного поста принимается, ошибки в ней - сообщением
	b.onChannelPost(ctx, edit(week, "19.10\nРабота", day(10, 20)), true)
	if latestText(t, b) != "19.10\nРабота" {
		t.Fatal("edit of the week post was not saved")
	}
	if texts := tg.sent(tg.take()); len(texts) != 1 || !strings.Contains(texts[0], "ошибки") {
		t.Fatalf("expected one error message, got %q", texts)
	}
}

func TestNextWeekPostAndLateFix(t *testing.T) {
	b, tg, _, _ := newTelegramBot(t)
	b.clock = func() time.Time { return time.Date(2026, 10, 28, 12, 0, 0, 0, tz) }
	ctx := context.Background()

	old := channelPost("19.10\nРабота - 8 часов", day(10, 19))
	b.onChannelPost(ctx, old, false)

	// Новый пост с опечаткой игнорируется, после исправления становится постом недели
	next := channelPost("26.10\nРабота", day(10, 26))
	b.onChannelPost(ctx, next, false)
	if latestText(t, b) != old.Text {
		t.Fatal("broken new post was accepted")
	}
	fixed := edit(next, "26.10\nРабота - 8 часов", day(10, 27))
	b.onChannelPost(ctx, fixed, true)
	if latestText(t, b) != fixed.Text {
		t.Fatalf("fixed new post not accepted, latest = %q", latestText(t, b))
	}

	// Правка поста прошлой недели сохраняется, но постом недели остаётся новый
	b.onChannelPost(ctx, edit(old, "19.10\nРабота - 7 часов", day(10, 28)), true)
	if latestText(t, b) != fixed.Text {
		t.Fatal("edit of last week's post became the week post")
	}
	stored, err := b.store.WeekPostByMessage(ctx, -100, int64(old.ID))
	if err != nil || stored.Text != "19.10\nРабота - 7 часов" {
		t.Fatalf("last week's post edit not saved: %+v %v", stored, err)
	}
	tg.take()
}

func TestStaleEditDoesNotRollBack(t *testing.T) {
	b, _, _, _ := newTelegramBot(t)
	ctx := context.Background()

	post := channelPost("19.10\nРабота - 8 часов", day(10, 19))
	b.onChannelPost(ctx, post, false)
	b.onChannelPost(ctx, edit(post, "19.10\nРабота - 7 часов", day(10, 21)), true)
	b.onChannelPost(ctx, edit(post, "19.10\nРабота - 6 часов", day(10, 20)), true)
	if got := latestText(t, b); got != "19.10\nРабота - 7 часов" {
		t.Fatalf("stale edit rolled the post back to %q", got)
	}
}

func TestPassphraseStartingWithSlash(t *testing.T) {
	b, tg, runner, st := newTelegramBot(t)
	ctx := context.Background()

	post := channelPost("19.10\nРабота - 8 часов\n\n20.10\nРабота - 8 часов", day(10, 19))
	b.onChannelPost(ctx, post, false)
	saved, err := st.LatestWeekPost(ctx, b.userID)
	if err != nil {
		t.Fatal(err)
	}
	b.pending = &pendingSubmit{postID: saved.ID, hash: hashText(saved.Text), promptID: 7, expires: fixedClock().Add(time.Minute)}

	msg := dm("/Str0ng-Passphrase-XYZ")
	b.onMessage(ctx, msg)

	if runner.passphrase != "/Str0ng-Passphrase-XYZ" {
		t.Fatalf("runner got passphrase %q", runner.passphrase)
	}
	calls := tg.take()
	deleted := map[string]bool{}
	for _, c := range calls {
		if c.method == "deleteMessage" {
			deleted[c.params["message_id"]] = true
		}
	}
	if !deleted[fmt.Sprint(msg.ID)] || !deleted["7"] {
		t.Fatalf("passphrase or prompt not deleted: %v", deleted)
	}
	for _, text := range tg.sent(calls) {
		if strings.Contains(text, "Str0ng") {
			t.Fatalf("passphrase echoed back: %q", text)
		}
	}
	subs, err := st.SubmissionsBetween(ctx, b.userID, day(10, 19), day(10, 25))
	if err != nil || len(subs) != 2 {
		t.Fatalf("submissions = %+v, %v; want 2", subs, err)
	}
	if b.pending != nil || b.running {
		t.Fatal("state not reset after submit")
	}
}

func TestExpiredPassphraseIsDeletedNotUsed(t *testing.T) {
	b, tg, runner, _ := newTelegramBot(t)
	b.pending = &pendingSubmit{postID: 1, hash: "x", promptID: 7, expires: fixedClock().Add(-time.Second)}

	b.onMessage(context.Background(), dm("late passphrase"))
	if runner.passphrase != "" {
		t.Fatal("expired passphrase was used")
	}
	if countMethod(tg.take(), "deleteMessage") != 2 {
		t.Fatal("expired passphrase or prompt not deleted")
	}
}

func TestStrayTextAndUnknownCommandsAreDeleted(t *testing.T) {
	b, tg, _, _ := newTelegramBot(t)
	for _, text := range []string{"просто текст", "/whatever"} {
		b.onMessage(context.Background(), dm(text))
		if countMethod(tg.take(), "deleteMessage") != 1 {
			t.Errorf("%q was not deleted", text)
		}
	}
}

func TestStrangersAreIgnored(t *testing.T) {
	b, tg, runner, _ := newTelegramBot(t)
	ctx := context.Background()
	b.pending = &pendingSubmit{postID: 1, hash: "x", promptID: 7, expires: fixedClock().Add(time.Minute)}

	stranger := &models.Message{ID: 1, From: &models.User{ID: 666}, Chat: models.Chat{ID: 666}, Text: "/status"}
	b.onMessage(ctx, stranger)
	// Владелец пишет в группе, а не в личке
	b.onMessage(ctx, &models.Message{ID: 2, From: &models.User{ID: 42}, Chat: models.Chat{ID: -555}, Text: "фраза"})
	// Пост в чужом канале
	b.onChannelPost(ctx, &models.Message{ID: 3, Chat: models.Chat{ID: -999}, Text: "19.10\nРабота - 8 часов"}, false)

	if calls := tg.take(); len(calls) != 0 {
		t.Fatalf("bot reacted to strangers: %+v", calls)
	}
	if runner.passphrase != "" || b.pending == nil {
		t.Fatal("stranger's message was taken as passphrase")
	}

	b.onCallback(ctx, &models.CallbackQuery{ID: "cb", From: models.User{ID: 666}, Data: "s:1:x:1"})
	calls := tg.take()
	if len(calls) != 1 || calls[0].method != "answerCallbackQuery" {
		t.Fatalf("stranger's callback: %+v", calls)
	}
}

func TestCancelWhilePending(t *testing.T) {
	b, tg, runner, _ := newTelegramBot(t)
	b.pending = &pendingSubmit{postID: 1, hash: "x", promptID: 7, expires: fixedClock().Add(time.Minute)}

	b.onMessage(context.Background(), dm("/cancel"))
	if b.pending != nil || runner.passphrase != "" {
		t.Fatal("/cancel did not cancel or was taken as passphrase")
	}
	tg.take()
}
