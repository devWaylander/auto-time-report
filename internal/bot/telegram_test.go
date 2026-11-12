package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"

	"github.com/devWaylander/auto-time-report/internal/corp"
	"github.com/devWaylander/auto-time-report/internal/secret"
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
	// Правка сообщения не проходит: его будто удалили в чате
	editsFail bool
	// Кнопку под постом канала повесить нельзя: у бота нет права редактировать сообщения
	markupFails bool
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
	editsFail, markupFails := f.editsFail, f.markupFails
	f.mu.Unlock()

	if markupFails && method == "editMessageReplyMarkup" {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 400, "description": "Bad Request: message can't be edited"})
		return
	}

	if editsFail && method == "editMessageText" {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 400, "description": "Bad Request: message to edit not found"})
		return
	}

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

// fakeRunner вместо корпоративной части: запоминает фразу и задание, "записывает" всё,
// кроме дней из inJira - те будто уже стоят в Jira руками
type fakeRunner struct {
	passphrase string
	jobs       []corp.Job
	inJira     map[string]time.Duration // дата 2006-01-02 -> сколько стоит в Jira
}

func (r *fakeRunner) Run(_ context.Context, passphrase []byte, job corp.Job, hooks corp.Hooks) (corp.Outcome, error) {
	r.passphrase = string(passphrase)
	r.jobs = append(r.jobs, job)
	out := corp.Outcome{}
	for i, w := range job.Worklogs {
		if d, ok := r.inJira[w.Date.Format("2006-01-02")]; ok {
			res := corp.WorklogResult{Job: w, Status: corp.StatusAlreadyInJira, InJira: d}
			hooks.Found(res)
			out.Worklogs = append(out.Worklogs, res)
			continue
		}
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
	// Имя бота как будто уже спросили у Telegram: getMe не мешается в проверках вызовов
	b.botURL = testBotURL
	tg.take()
	return b, tg, runner, st
}

var nextMessageID = 100

const testBotURL = "https://t.me/bot"

// channelPost - сообщение канала. Кнопка возврата в бота под ним уже есть, как у поста недели после первой
// правки: тесты про неё используют bareChannelPost
func channelPost(text string, posted time.Time) *models.Message {
	m := bareChannelPost(text, posted)
	m.ReplyMarkup = &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{{{Text: "🤖 В бота", URL: testBotURL}}}}
	return m
}

func bareChannelPost(text string, posted time.Time) *models.Message {
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

	week := channelPost("19.10\nРабота - 8ч", monday)
	b.onChannelPost(ctx, week, false)
	if latestText(t, b) != week.Text {
		t.Fatal("week post not accepted")
	}

	// Заметки: ключ отдельной строкой, дата посреди недели, обычный текст
	for _, note := range []string{
		"ABC-123\nспросить у аналитика про лимиты",
		"21.10\nСозвон с командой - 1ч",
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
	calls := tg.take()
	if texts := tg.sent(calls); len(texts) != 1 || !strings.Contains(texts[0], "ошибки") {
		t.Fatalf("expected one error message, got %q", texts)
	}
	if markup := lastParam(calls, "sendMessage", "reply_markup"); !strings.Contains(markup, viewStatus) {
		t.Errorf("error message has no refresh button: %s", markup)
	}
}

// saved - последний сохранённый пост
func saved(t *testing.T, b *Bot) store.WeekPost {
	t.Helper()
	p, err := b.store.LatestWeekPost(context.Background(), b.userID)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// liveID - живое сообщение последнего поста; пусто - его нет
func liveID(t *testing.T, b *Bot) string {
	t.Helper()
	l, ok := b.live[saved(t, b).ID]
	if !ok {
		return ""
	}
	return fmt.Sprint(l.messageID)
}

// inPlace - сообщение messageID обновлено на месте: ни новых, ни удалённых сообщений
func inPlace(calls []apiCall, messageID, want string) bool {
	return countMethod(calls, "sendMessage") == 0 && countMethod(calls, "deleteMessage") == 0 &&
		lastParam(calls, "editMessageText", "message_id") == messageID &&
		strings.Contains(lastParam(calls, "editMessageText", "text"), want)
}

// replaced - старое живое сообщение удалено, внизу новое с want
func replaced(calls []apiCall, old, want string) bool {
	return lastParam(calls, "deleteMessage", "message_id") == old && countMethod(calls, "sendMessage") == 1 &&
		strings.Contains(lastParam(calls, "sendMessage", "text"), want)
}

// У поста одно живое сообщение: последнее, что бот о нём показал. Правки обновляют его на месте, новая
// проблема приходит новым сообщением внизу, а старое удаляется
func TestLiveMessage(t *testing.T) {
	b, tg, _, _ := newTelegramBot(t)
	ctx := context.Background()
	week := channelPost("19.10\nРабота - 8ч", day(10, 19))
	b.onChannelPost(ctx, week, false)
	tg.take()
	editTo := func(text string) []apiCall {
		b.onChannelPost(ctx, edit(week, text, day(10, 20)), true)
		return tg.take()
	}
	press := func(data, messageID string) []apiCall {
		id, _ := strconv.Atoi(messageID)
		b.onCallback(ctx, &models.CallbackQuery{
			ID: "cb", From: models.User{ID: b.cfg.OwnerID}, Data: data,
			Message: models.MaybeInaccessibleMessage{Message: &models.Message{ID: id}},
		})
		return tg.take()
	}

	// Живого сообщения ещё нет: чистая правка - тишина
	if calls := editTo("19.10\nРабота - 7ч"); len(calls) != 0 {
		t.Fatalf("clean edit without a live message: %+v", calls)
	}
	// /status становится живым, правка обновляет его на месте
	b.onMessage(ctx, dm("/status"))
	tg.take()
	a := liveID(t, b)
	if calls := editTo("19.10\nРабота - 3ч"); !inPlace(calls, a, "3ч") {
		t.Fatalf("/status was not updated by the edit: %+v", calls)
	}
	// Новая проблема - внизу новое сообщение со статусом и предупреждением, старое удалено
	calls := editTo("19.10\nРабота - 3ч\n\n20.10")
	if !replaced(calls, a, "20.10: дата есть") || !strings.Contains(lastParam(calls, "sendMessage", "text"), "Неделя") {
		t.Fatalf("new problem did not come as a new message: %+v", calls)
	}
	// Та же проблема при следующей правке - обновление на месте
	c := liveID(t, b)
	if calls := editTo("19.10\nРабота - 4ч\n\n20.10"); !inPlace(calls, c, "4ч") {
		t.Fatalf("unchanged problem was not updated in place: %+v", calls)
	}
	// Ошибка разбора - снова новое сообщение
	if calls := editTo("19.10\nРабота\n\n20.10"); !replaced(calls, c, "ошибки") {
		t.Fatalf("parse error did not come as a new message: %+v", calls)
	}
	e := liveID(t, b)
	if markup := lastParam(calls, "sendMessage", "reply_markup"); markup == "" {
		t.Fatal("error message has no buttons")
	}
	// "Обновить" на нём пересчитывает его на месте, а исправление ставит туда статус
	if calls := press(viewStatus, e); !inPlace(calls, e, "ошибки") || liveID(t, b) != e {
		t.Fatalf("refresh of the error message: %+v", calls)
	}
	if calls := editTo("19.10\nРабота - 8ч"); !inPlace(calls, e, "Неделя") {
		t.Fatalf("fixed post did not show the status in place: %+v", calls)
	}
	// /submit становится живым и остаётся с кнопками записи после правок
	b.onMessage(ctx, dm("/submit"))
	calls = tg.take()
	if lastParam(calls, "deleteMessage", "message_id") != e {
		t.Fatalf("previous live message not deleted: %+v", calls)
	}
	s := liveID(t, b)
	calls = editTo("19.10\nРабота - 6ч")
	if !inPlace(calls, s, "6ч") || !strings.Contains(lastParam(calls, "editMessageText", "reply_markup"), submitPrefix) {
		t.Fatalf("submit preview lost its mode: %+v", calls)
	}
	// Сообщение удалили в чате: бот перестаёт его вести, следующая проблема придёт новым сообщением
	tg.mu.Lock()
	tg.editsFail = true
	tg.mu.Unlock()
	editTo("19.10\nРабота - 5ч")
	if liveID(t, b) != "" {
		t.Fatal("deleted message is still live")
	}
	calls = editTo("19.10\nРабота")
	if countMethod(calls, "deleteMessage") != 0 || countMethod(calls, "sendMessage") != 1 {
		t.Fatalf("problem after the live message was deleted: %+v", calls)
	}
	tg.mu.Lock()
	tg.editsFail = false
	tg.mu.Unlock()
	// "Скрыть" убирает сообщение, и бот его больше не трогает
	b.onMessage(ctx, dm("/submit"))
	tg.take()
	press(hideData, liveID(t, b))
	if calls := editTo("19.10\nРабота - 8ч"); len(calls) != 0 {
		t.Fatalf("hidden message was touched: %+v", calls)
	}
}

// Живое сообщение поста прошлой недели после исправления показывает статус той недели, без кнопок отправки
func TestOldWeekLiveMessage(t *testing.T) {
	b, tg, _, _ := newTelegramBot(t)
	b.clock = func() time.Time { return time.Date(2026, 10, 28, 12, 0, 0, 0, tz) }
	ctx := context.Background()
	old := channelPost("19.10\nРабота - 8ч", day(10, 19))
	b.onChannelPost(ctx, old, false)
	b.onChannelPost(ctx, channelPost("26.10\nРабота - 8ч", day(10, 26)), false)
	b.onChannelPost(ctx, edit(old, "19.10\nРабота", day(10, 27)), true)
	calls := tg.take()
	if len(tg.sent(calls)) != 1 {
		t.Fatalf("no message for last week's post: %+v", calls)
	}
	var id int
	for _, l := range b.live {
		id = l.messageID
	}
	b.onChannelPost(ctx, edit(old, "19.10\nРабота - 7ч", day(10, 28)), true)
	if calls := tg.take(); !inPlace(calls, fmt.Sprint(id), "Неделя 19.10") ||
		strings.Contains(lastParam(calls, "editMessageText", "reply_markup"), submitPrefix) {
		t.Fatalf("last week's message: %+v", calls)
	}
}

// Правка известного поста принимается, даже если первая строка перестала быть датой или ключом задачи
func TestKnownPostEditWithHeading(t *testing.T) {
	b, tg, _, _ := newTelegramBot(t)
	ctx := context.Background()
	week := channelPost("19.10\nРабота - 8ч", day(10, 19))
	b.onChannelPost(ctx, week, false)
	tg.take()

	b.onChannelPost(ctx, edit(week, "Неделя 43\n\n19.10\nРабота - 8ч", day(10, 20)), true)
	if latestText(t, b) != "Неделя 43\n\n19.10\nРабота - 8ч" {
		t.Fatal("edit with a heading was ignored")
	}
	if texts := tg.sent(tg.take()); len(texts) != 1 || !strings.Contains(texts[0], "ошибки") {
		t.Fatalf("expected an error notice, got %q", texts)
	}
}

// Новый пост принимается без ошибок, но с пустой датой - об этом сразу уведомление
func TestNewPostWithEmptyDateNotice(t *testing.T) {
	b, tg, _, _ := newTelegramBot(t)
	b.onChannelPost(context.Background(), channelPost("19.10\nРабота - 8ч\n\n20.10", day(10, 19)), false)
	if texts := tg.sent(tg.take()); len(texts) != 1 || !strings.Contains(texts[0], "20.10: дата есть") {
		t.Fatalf("expected an empty date notice, got %q", texts)
	}
}

func TestNextWeekPostAndLateFix(t *testing.T) {
	b, tg, _, _ := newTelegramBot(t)
	b.clock = func() time.Time { return time.Date(2026, 10, 28, 12, 0, 0, 0, tz) }
	ctx := context.Background()

	old := channelPost("19.10\nРабота - 8ч", day(10, 19))
	b.onChannelPost(ctx, old, false)

	// Новый пост с опечаткой игнорируется, после исправления становится постом недели
	next := channelPost("26.10\nРабота", day(10, 26))
	b.onChannelPost(ctx, next, false)
	if latestText(t, b) != old.Text {
		t.Fatal("broken new post was accepted")
	}
	fixed := edit(next, "26.10\nРабота - 8ч", day(10, 27))
	b.onChannelPost(ctx, fixed, true)
	if latestText(t, b) != fixed.Text {
		t.Fatalf("fixed new post not accepted, latest = %q", latestText(t, b))
	}

	// Правка поста прошлой недели сохраняется, но постом недели остаётся новый
	b.onChannelPost(ctx, edit(old, "19.10\nРабота - 7ч", day(10, 28)), true)
	if latestText(t, b) != fixed.Text {
		t.Fatal("edit of last week's post became the week post")
	}
	stored, err := b.store.WeekPostByMessage(ctx, -100, int64(old.ID))
	if err != nil || stored.Text != "19.10\nРабота - 7ч" {
		t.Fatalf("last week's post edit not saved: %+v %v", stored, err)
	}
	tg.take()
}

func TestStaleEditDoesNotRollBack(t *testing.T) {
	b, _, _, _ := newTelegramBot(t)
	ctx := context.Background()

	post := channelPost("19.10\nРабота - 8ч", day(10, 19))
	b.onChannelPost(ctx, post, false)
	b.onChannelPost(ctx, edit(post, "19.10\nРабота - 7ч", day(10, 21)), true)
	b.onChannelPost(ctx, edit(post, "19.10\nРабота - 6ч", day(10, 20)), true)
	if got := latestText(t, b); got != "19.10\nРабота - 7ч" {
		t.Fatalf("stale edit rolled the post back to %q", got)
	}
}

func TestPassphraseStartingWithSlash(t *testing.T) {
	b, tg, runner, st := newTelegramBot(t)
	ctx := context.Background()

	post := channelPost("19.10\nРабота - 8ч\n\n20.10\nРабота - 8ч", day(10, 19))
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
	if !strings.Contains(lastParam(calls, "sendMessage", "reply_markup"), viewNew) {
		t.Error("outcome has no status button")
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

// Запрос фразы не висит дольше срока: по таймеру он удаляется, а сообщение после этого фразой не считается
func TestPendingExpiresByTimer(t *testing.T) {
	b, tg, runner, _ := newTelegramBot(t)
	p := &pendingSubmit{postID: 1, hash: "x", promptID: 7, expires: fixedClock().Add(time.Minute)}
	b.pending = p

	// Таймер от прежнего нажатия не трогает новое ожидание
	b.expirePending(&pendingSubmit{promptID: 6})
	if b.pending != p || len(tg.take()) != 0 {
		t.Fatal("stale timer touched the current prompt")
	}

	b.expirePending(p)
	calls := tg.take()
	if b.pending != nil || lastParam(calls, "deleteMessage", "message_id") != "7" {
		t.Fatalf("prompt not expired: %+v", calls)
	}
	if texts := tg.sent(calls); len(texts) != 1 || !strings.Contains(texts[0], "истекло") {
		t.Fatalf("expected expiry notice, got %q", texts)
	}
	b.onMessage(context.Background(), dm("late passphrase"))
	if runner.passphrase != "" {
		t.Fatal("message after expiry was used as passphrase")
	}
	tg.take()
}

// Повторное нажатие отправки заменяет запрос фразы: прежний удаляется
func TestSecondSubmitReplacesPrompt(t *testing.T) {
	b, tg, _, st := newTelegramBot(t)
	ctx := context.Background()
	b.onChannelPost(ctx, channelPost("19.10\nРабота - 8ч", day(10, 19)), false)
	saved, err := st.LatestWeekPost(ctx, b.userID)
	if err != nil {
		t.Fatal(err)
	}
	b.pending = &pendingSubmit{postID: saved.ID, hash: hashText(saved.Text), promptID: 7, expires: fixedClock().Add(time.Minute)}
	tg.take()

	b.onCallback(ctx, &models.CallbackQuery{
		ID: "cb", From: models.User{ID: b.cfg.OwnerID}, Data: fmt.Sprintf("%s%d:%s:0", submitPrefix, saved.ID, hashText(saved.Text)),
	})
	calls := tg.take()
	if lastParam(calls, "deleteMessage", "message_id") != "7" || b.pending == nil || b.pending.promptID == 7 {
		t.Fatalf("old prompt not replaced: %+v", calls)
	}
	b.pending = nil
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
	b.onChannelPost(ctx, &models.Message{ID: 3, Chat: models.Chat{ID: -999}, Text: "19.10\nРабота - 8ч"}, false)

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

// Все запросы к Bot API идут через прокси из конфига, а его пароль не попадает в логи
func TestTelegramProxy(t *testing.T) {
	tg := &fakeTelegram{}
	var proxied atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") == "" {
			w.WriteHeader(http.StatusProxyAuthRequired)
			return
		}
		proxied.Add(1)
		tg.ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)

	base, st := newTestBot(t, nil)
	cfg := base.cfg
	// Сам Bot API недоступен: без прокси бот не стартует
	cfg.ServerURL = "http://telegram.invalid"
	cfg.ProxyURL = secret.String("http://bot:s3cret@" + strings.TrimPrefix(proxy.URL, "http://"))
	b, err := New(cfg, st, base.cal, &fakeRunner{}, base.userID)
	if err != nil {
		t.Fatal(err)
	}
	if proxied.Load() == 0 || countMethod(tg.take(), "getMe") != 1 {
		t.Fatalf("getMe did not go through the proxy: proxied=%d", proxied.Load())
	}
	if got := b.redact("proxyconnect: http://bot:s3cret@host"); strings.Contains(got, "s3cret") {
		t.Fatalf("redact = %q", got)
	}

	cfg.ProxyURL = "ss://bot:s3cret@host:1"
	if _, err := New(cfg, st, base.cal, &fakeRunner{}, base.userID); err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("bad proxy: err = %v", err)
	}
}

// Меню команд выставляется только в личке владельца, и каждая команда из него обрабатывается,
// а не удаляется как неизвестная
func TestCommandMenu(t *testing.T) {
	b, tg, _, _ := newTelegramBot(t)
	b.setCommands(context.Background())
	calls := tg.take()
	if countMethod(calls, "setMyCommands") != 1 {
		t.Fatalf("calls = %+v", calls)
	}
	var set apiCall
	for _, c := range calls {
		if c.method == "setMyCommands" {
			set = c
		}
	}
	if !strings.Contains(set.params["scope"], `"type":"chat"`) ||
		!strings.Contains(set.params["scope"], fmt.Sprintf(`"chat_id":%d`, b.cfg.OwnerID)) {
		t.Errorf("scope = %s", set.params["scope"])
	}
	var menu []models.BotCommand
	if err := json.Unmarshal([]byte(set.params["commands"]), &menu); err != nil {
		t.Fatal(err)
	}
	if len(menu) != len(commands) {
		t.Fatalf("menu = %+v", menu)
	}

	for _, c := range menu {
		if c.Command != "help" && !strings.Contains(helpText, "/"+c.Command) {
			t.Errorf("/%s is not in /help", c.Command)
		}
		b.onMessage(context.Background(), dm("/"+c.Command))
		if countMethod(tg.take(), "deleteMessage") != 0 {
			t.Errorf("/%s is deleted as unknown", c.Command)
		}
	}
}

// /status -> "К отправке" перерисовывает то же сообщение с кнопками записи, "Статус" присылает новое
func TestPreviewButtons(t *testing.T) {
	b, tg, _, _ := newTelegramBot(t)
	ctx := context.Background()
	b.onChannelPost(ctx, channelPost("19.10\nРабота - 8ч", day(10, 19)), false)
	tg.take()

	b.onMessage(ctx, dm("/status"))
	calls := tg.take()
	if countMethod(calls, "sendMessage") != 1 || !strings.Contains(lastParam(calls, "sendMessage", "reply_markup"), viewSubmit) {
		t.Fatalf("/status has no way to submit: %+v", calls)
	}

	press := func(data string) []apiCall {
		b.onCallback(ctx, &models.CallbackQuery{
			ID: "cb", From: models.User{ID: b.cfg.OwnerID}, Data: data,
			Message: models.MaybeInaccessibleMessage{Message: &models.Message{ID: 55}},
		})
		return tg.take()
	}

	calls = press(viewSubmit)
	if countMethod(calls, "sendMessage") != 0 || lastParam(calls, "editMessageText", "message_id") != "55" ||
		!strings.Contains(lastParam(calls, "editMessageText", "reply_markup"), submitPrefix) {
		t.Fatalf("submit view did not replace the message: %+v", calls)
	}

	calls = press(viewNew)
	if countMethod(calls, "editMessageText") != 0 || countMethod(calls, "sendMessage") != 1 {
		t.Fatalf("status button did not send a new preview: %+v", calls)
	}
}

func lastParam(calls []apiCall, method, param string) string {
	v := ""
	for _, c := range calls {
		if c.method == method {
			v = c.params[param]
		}
	}
	return v
}

// День, поставленный в Jira руками, бот запоминает: превью считает его записанным, а расхождение с постом
// видно в итоге отправки
func TestWorklogFoundInJiraIsRemembered(t *testing.T) {
	b, tg, runner, st := newTelegramBot(t)
	ctx := context.Background()
	runner.inJira = map[string]time.Duration{"2026-10-19": 6 * time.Hour}

	b.onChannelPost(ctx, channelPost("19.10\nРабота - 8ч\n\n20.10\nРабота - 8ч", day(10, 19)), false)
	saved, err := st.LatestWeekPost(ctx, b.userID)
	if err != nil {
		t.Fatal(err)
	}
	tg.take()

	b.pending = &pendingSubmit{postID: saved.ID, hash: hashText(saved.Text), promptID: 7, expires: fixedClock().Add(time.Minute)}
	b.onMessage(ctx, dm("Str0ng-Passphrase-XYZ"))
	outcome := strings.Join(tg.sent(tg.take()), "\n")
	if !strings.Contains(outcome, "уже было в Jira вручную, пропущено. ⚠️ В Jira 6ч, в посте 8ч") {
		t.Fatalf("outcome does not show the mismatch:\n%s", outcome)
	}

	subs, err := st.SubmissionsBetween(ctx, b.userID, day(10, 19), day(10, 25))
	if err != nil || len(subs) != 2 || !subs[0].Manual || subs[0].Duration != 6*time.Hour || subs[1].Manual {
		t.Fatalf("submissions = %+v, %v", subs, err)
	}

	b.onMessage(ctx, dm("/status"))
	preview := strings.Join(tg.sent(tg.take()), "\n")
	if strings.Contains(preview, "К записи в Jira") || !strings.Contains(preview, "⚠️ Пн 19.10 - 8ч вручную · в Jira 6ч") {
		t.Fatalf("manual day is not counted as written or its mismatch is hidden:\n%s", preview)
	}

	// Правка поста по такому дню - то же предупреждение, что и для записей бота
	b.onChannelPost(ctx, edit(&models.Message{ID: int(saved.MessageID), Chat: models.Chat{ID: saved.ChatID}},
		"19.10\nРабота - 7ч\n\n20.10\nРабота - 8ч", day(10, 21)), true)
	if warn := strings.Join(tg.sent(tg.take()), "\n"); !strings.Contains(warn, "в Jira вручную 6ч, в посте теперь 7ч") {
		t.Fatalf("no warning for the manual day:\n%s", warn)
	}
}

// Часть дня уже в Jira: в отправку уходит только недостающий worklog, записанный не трогается
func TestSubmitOnlyMissingWorklogOfPartialDay(t *testing.T) {
	b, tg, runner, st := newTelegramBot(t)
	ctx := context.Background()
	b.onChannelPost(ctx, channelPost("20.10\nРабота - 6ч\n\nXYZ-101\n20.10\nСозвон - 2ч", day(10, 20)), false)
	saved, err := st.LatestWeekPost(ctx, b.userID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveSubmission(ctx, store.Submission{
		UserID: b.userID, WeekPostID: saved.ID, IssueKey: "ABC-1", WorkDate: day(10, 20),
		Duration: 6 * time.Hour, Comment: "Работа - 6ч", WorklogID: 1, PeriodID: 228,
	}); err != nil {
		t.Fatal(err)
	}
	b.onMessage(ctx, dm("/submit"))
	tg.take()
	live := fmt.Sprint(b.live[saved.ID].messageID)

	b.pending = &pendingSubmit{postID: saved.ID, hash: hashText(saved.Text), promptID: 7, expires: fixedClock().Add(time.Minute)}
	b.onMessage(ctx, dm("Str0ng-Passphrase-XYZ"))
	calls := tg.take()
	// После отправки живой предпросмотр показывает свежее состояние на месте
	if lastParam(calls, "editMessageText", "message_id") != live || !strings.Contains(lastParam(calls, "editMessageText", "text"), "☑️ Вт 20.10 - 8ч") {
		t.Fatalf("live preview not refreshed after submit: %+v", calls)
	}
	if len(runner.jobs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(runner.jobs))
	}
	got := runner.jobs[0].Worklogs
	if len(got) != 1 || got[0].IssueKey != "XYZ-101" || got[0].Duration != 2*time.Hour || !got[0].Date.Equal(day(10, 20)) {
		t.Fatalf("job worklogs = %+v, want only XYZ-101 20.10 2ч", got)
	}

	b.onMessage(ctx, dm("/status"))
	if preview := strings.Join(tg.sent(tg.take()), "\n"); !strings.Contains(preview, "☑️ Вт 20.10 - 8ч") || strings.Contains(preview, "К записи") {
		t.Fatalf("day is not fully written after submit:\n%s", preview)
	}
}

// Под постом недели бот вешает кнопку возврата в бота - один раз, только под постом недели. Telegram не дал -
// больше не пытается
func TestBotButtonUnderWeekPost(t *testing.T) {
	b, tg, _, _ := newTelegramBot(t)
	ctx := context.Background()

	b.onChannelPost(ctx, bareChannelPost("Купить молоко", day(10, 19)), false)
	if calls := tg.take(); len(calls) != 0 {
		t.Fatalf("note got a button: %+v", calls)
	}
	week := bareChannelPost("19.10\nРабота - 8ч", day(10, 19))
	b.onChannelPost(ctx, week, false)
	calls := tg.take()
	if countMethod(calls, "editMessageReplyMarkup") != 1 || lastParam(calls, "editMessageReplyMarkup", "message_id") != fmt.Sprint(week.ID) ||
		!strings.Contains(lastParam(calls, "editMessageReplyMarkup", "reply_markup"), testBotURL) {
		t.Fatalf("week post got no button: %+v", calls)
	}
	// Правка уже с кнопкой - повторно не вешаем
	withButton := edit(week, "19.10\nРабота - 7ч", day(10, 20))
	withButton.ReplyMarkup = channelPost("", day(10, 20)).ReplyMarkup
	b.onChannelPost(ctx, withButton, true)
	if calls := tg.take(); countMethod(calls, "editMessageReplyMarkup") != 0 {
		t.Fatalf("button attached again: %+v", calls)
	}
	// Telegram не дал повесить кнопку - бот больше не пытается
	tg.mu.Lock()
	tg.markupFails = true
	tg.mu.Unlock()
	b.onChannelPost(ctx, edit(week, "19.10\nРабота - 6ч", day(10, 21)), true)
	tg.take()
	b.onChannelPost(ctx, edit(week, "19.10\nРабота - 5ч", day(10, 22)), true)
	if calls := tg.take(); countMethod(calls, "editMessageReplyMarkup") != 0 {
		t.Fatalf("bot keeps trying after a refusal: %+v", calls)
	}
}
