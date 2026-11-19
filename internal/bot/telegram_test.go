package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"

	"github.com/devWaylander/auto-time-report/internal/corp"
	"github.com/devWaylander/auto-time-report/internal/notify"
	"github.com/devWaylander/auto-time-report/internal/secret"
	"github.com/devWaylander/auto-time-report/internal/store"
)

// apiCall - один запрос бота к Bot API
type apiCall struct {
	method string
	params map[string]string
}

func (c apiCall) text() string {
	if raw := c.params["rich_message"]; raw != "" {
		var rich models.InputRichMessage
		if err := json.Unmarshal([]byte(raw), &rich); err == nil {
			return rich.HTML
		}
	}
	return c.params["text"]
}

func isMessageSend(method string) bool {
	return method == "sendMessage" || method == "sendRichMessage"
}

// fakeTelegram записывает вызовы Bot API и отвечает успехом
type fakeTelegram struct {
	mu       sync.Mutex
	calls    []apiCall
	nextID   int
	chatInfo models.ChatFullInfo
	// Правка сообщения не проходит: его будто удалили в чате
	editsFail bool
	// Telegram больше не даёт удалить старое сообщение
	deletesFail bool
	// Telegram не принимает новое сообщение
	sendsFail bool
	// Кнопку под постом канала повесить нельзя: у бота нет права редактировать сообщения
	markupFails bool
	// Telegram отклоняет отправку HTML-виджета
	widgetDenied bool
}

func (f *fakeTelegram) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	params := map[string]string{}
	_ = r.ParseMultipartForm(1 << 20)
	for k, v := range r.PostForm {
		params[k] = v[0]
	}

	f.mu.Lock()
	f.calls = append(f.calls, apiCall{method: method, params: params})
	f.nextID++
	id := f.nextID
	chatInfo := f.chatInfo
	editsFail, deletesFail, sendsFail, markupFails := f.editsFail, f.deletesFail, f.sendsFail, f.markupFails
	widgetDenied := f.widgetDenied
	f.mu.Unlock()

	if markupFails && method == "editMessageReplyMarkup" {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 400, "description": "Bad Request: message can't be edited"})
		return
	}

	if editsFail && method == "editMessageText" {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 400, "description": "Bad Request: message to edit not found"})
		return
	}
	if deletesFail && method == "deleteMessage" {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 400, "description": "Bad Request: message can't be deleted"})
		return
	}
	if sendsFail && isMessageSend(method) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 500, "description": "Internal Server Error"})
		return
	}
	if widgetDenied && method == "sendWidget" {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 400, "description": "Bad Request: INPUT_MEDIA_WIDGET_NOT_ALLOWED"})
		return
	}

	var result any = true
	switch method {
	case "getMe":
		result = map[string]any{"id": 1, "is_bot": true, "first_name": "bot", "username": "bot"}
	case "getChat":
		result = chatInfo
	case "sendMessage", "sendRichMessage", "sendWidget", "editMessageText", "editMessageReplyMarkup":
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
		if isMessageSend(c.method) || c.method == "editMessageText" {
			texts = append(texts, c.text())
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
	if job.RequestApproval {
		out.Approved = slices.Clone(job.KnownPeriods)
		for _, worklog := range out.Worklogs {
			if worklog.Status == corp.StatusCreated && !slices.Contains(out.Approved, worklog.PeriodID) {
				out.Approved = append(out.Approved, worklog.PeriodID)
			}
		}
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

// liveID - активное сообщение, пусто если его нет
func liveID(b *Bot) string {
	if b.active.MessageID == 0 {
		return ""
	}
	return fmt.Sprint(b.active.MessageID)
}

// inPlace - сообщение messageID обновлено на месте: ни новых, ни удалённых сообщений
func inPlace(calls []apiCall, messageID, want string) bool {
	return countMethod(calls, "sendMessage") == 0 && countMethod(calls, "sendRichMessage") == 0 && countMethod(calls, "deleteMessage") == 0 &&
		lastParam(calls, "editMessageText", "message_id") == messageID &&
		strings.Contains(lastParam(calls, "editMessageText", "text"), want)
}

// replaced - старое живое сообщение удалено, внизу новое с want
func replaced(calls []apiCall, old, want string) bool {
	return lastParam(calls, "deleteMessage", "message_id") == old && countMethod(calls, "sendMessage")+countMethod(calls, "sendRichMessage") == 1 &&
		strings.Contains(lastParam(calls, "sendMessage", "text")+lastParam(calls, "sendRichMessage", "text"), want)
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
	a := liveID(b)
	if calls := editTo("19.10\nРабота - 3ч"); !inPlace(calls, a, "3ч") {
		t.Fatalf("/status was not updated by the edit: %+v", calls)
	}
	// Новая проблема - внизу новое сообщение со статусом и предупреждением, старое удалено
	calls := editTo("19.10\nРабота - 3ч\n\n20.10")
	if !replaced(calls, a, "20.10: дата есть") || !strings.Contains(lastParam(calls, "sendRichMessage", "text"), "Неделя") {
		t.Fatalf("new problem did not come as a new message: %+v", calls)
	}
	// Та же проблема при следующей правке - обновление на месте
	c := liveID(b)
	if calls := editTo("19.10\nРабота - 4ч\n\n20.10"); !inPlace(calls, c, "4ч") {
		t.Fatalf("unchanged problem was not updated in place: %+v", calls)
	}
	// Ошибка разбора - снова новое сообщение
	calls = editTo("19.10\nРабота\n\n20.10")
	if !replaced(calls, c, "ошибки") {
		t.Fatalf("parse error did not come as a new message: %+v", calls)
	}
	e := liveID(b)
	if markup := lastParam(calls, "sendMessage", "reply_markup"); markup == "" {
		t.Fatal("error message has no buttons")
	}
	// "Обновить" на нём пересчитывает его на месте, а исправление ставит туда статус
	if calls := press(viewStatus, e); !inPlace(calls, e, "ошибки") || liveID(b) != e {
		t.Fatalf("refresh of the error message: %+v", calls)
	}
	if calls := editTo("19.10\nРабота - 8ч"); !inPlace(calls, e, "Неделя") {
		t.Fatalf("fixed post did not show the status in place: %+v", calls)
	}
	// /submit становится живым и остаётся с кнопками записи после правок
	b.onMessage(ctx, dm("/submit"))
	calls = tg.take()
	if !slices.ContainsFunc(calls, func(c apiCall) bool {
		return c.method == "deleteMessage" && c.params["message_id"] == e
	}) {
		t.Fatalf("previous live message not deleted: %+v", calls)
	}
	s := liveID(b)
	calls = editTo("19.10\nРабота - 6ч")
	if !inPlace(calls, s, "6ч") || !strings.Contains(lastParam(calls, "editMessageText", "reply_markup"), submitPrefix) {
		t.Fatalf("submit preview lost its mode: %+v", calls)
	}
	// Сообщение удалили в чате: бот перестаёт его вести, следующая проблема придёт новым сообщением
	tg.mu.Lock()
	tg.editsFail = true
	tg.mu.Unlock()
	editTo("19.10\nРабота - 5ч")
	if liveID(b) != "" {
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
	press(hideData, liveID(b))
	if calls := editTo("19.10\nРабота - 8ч"); len(calls) != 0 {
		t.Fatalf("hidden message was touched: %+v", calls)
	}
}

func TestOldWeekEditsAreIgnored(t *testing.T) {
	b, tg, _, _ := newTelegramBot(t)
	ctx := context.Background()
	old := channelPost("19.10\nРабота - 8ч", day(10, 19))
	b.onChannelPost(ctx, old, false)
	b.clock = func() time.Time { return day(10, 28) }
	b.onChannelPost(ctx, channelPost("26.10\nРабота - 8ч", day(10, 26)), false)
	tg.take()
	b.onChannelPost(ctx, edit(old, "19.10\nРабота", day(10, 27)), true)
	if calls := tg.take(); len(calls) != 0 {
		t.Fatalf("the old edit triggered Telegram calls: %+v", calls)
	}
	post, err := b.store.WeekPostByMessage(ctx, old.Chat.ID, int64(old.ID))
	if err != nil || post.Text != old.Text {
		t.Fatalf("the old edit changed stored text: %+v, %v", post, err)
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
	ctx := context.Background()

	old := channelPost("19.10\nРабота - 8ч", day(10, 19))
	b.onChannelPost(ctx, old, false)
	b.clock = func() time.Time { return day(10, 28) }
	// Правка прошлой недели с новой датой не переводит старый пост в текущую неделю
	b.onChannelPost(ctx, edit(old, "19.10\nРабота - 8ч\n\n26.10\nСозвон - 1ч", day(10, 26)), true)

	// Новый пост определяется по дате, ошибку во времени бот показывает в личке
	tg.take()
	next := channelPost("26.10\nРабота - 8 часов", day(10, 26))
	b.onChannelPost(ctx, next, false)
	if latestText(t, b) != next.Text {
		t.Fatal("new post was ignored because of a format error")
	}
	calls := tg.take()
	if text := lastParam(calls, "sendMessage", "text"); !strings.Contains(text, "ожидается формат времени") ||
		!strings.Contains(text, "26.10") || strings.Contains(lastParam(calls, "sendMessage", "reply_markup"), `"callback_data":"`+submitPrefix) {
		t.Fatalf("new post does not show the format error: %+v", calls)
	}
	activeID := liveID(b)
	fixed := edit(next, "26.10\nРабота - 8ч", day(10, 27))
	b.onChannelPost(ctx, fixed, true)
	if latestText(t, b) != fixed.Text {
		t.Fatalf("fixed new post not accepted, latest = %q", latestText(t, b))
	}
	if calls := tg.take(); !inPlace(calls, activeID, "Неделя 26.10") {
		t.Fatalf("fix did not update the same preview: %+v", calls)
	}

	// Правка старого поста не меняет ни текущую неделю, ни сохранённый текст
	b.onChannelPost(ctx, edit(old, "19.10\nРабота - 7ч", day(10, 28)), true)
	if latestText(t, b) != fixed.Text {
		t.Fatal("edit of last week's post became the week post")
	}
	stored, err := b.store.WeekPostByMessage(ctx, -100, int64(old.ID))
	if err != nil || stored.Text != old.Text {
		t.Fatalf("last week's post was changed: %+v %v", stored, err)
	}
	tg.take()
}

func TestDateStartsNewWeekPost(t *testing.T) {
	b, tg, _, _ := newTelegramBot(t)
	ctx := context.Background()
	b.clock = func() time.Time { return day(11, 13) }
	old := channelPost("09.11\nРевью кода - 8ч", day(11, 9))
	old.Chat.ID = -1001234567890
	b.cfg.ChannelID = old.Chat.ID
	b.onChannelPost(ctx, old, false)
	b.onMessage(ctx, dm("/status"))
	live := liveID(b)
	tg.take()
	b.clock = func() time.Time { return day(11, 18) }

	for _, text := range []string{"Заметка о встрече\n16.11", "13.11\nРевью кода - 8ч\n16.11", "23.11"} {
		note := channelPost(text, day(11, 18))
		note.Chat.ID = old.Chat.ID
		b.onChannelPost(ctx, note, false)
		if latestText(t, b) != old.Text || len(tg.take()) != 0 {
			t.Fatalf("the note %q replaced the week post", text)
		}
	}

	next := channelPost("16.11", day(11, 18))
	next.Chat.ID = old.Chat.ID
	b.onChannelPost(ctx, next, false)
	calls := tg.take()
	if latestText(t, b) != next.Text || !inPlace(calls, live, emptyWeekPostText) {
		t.Fatalf("the date did not start the current week post: %+v", calls)
	}
	if savedID := b.active.PostID; savedID == 0 {
		t.Fatal("the new post is not tracked")
	}

	// Второй пост той же недели не перехватывает уже начатый
	duplicate := channelPost("18.11\nРабота - 8ч", day(11, 18))
	duplicate.Chat.ID = old.Chat.ID
	b.onChannelPost(ctx, duplicate, false)
	if latestText(t, b) != next.Text || len(tg.take()) != 0 {
		t.Fatal("another post replaced the current week")
	}

	b.onMessage(ctx, dm("/submit"))
	calls = tg.take()
	if text := lastParam(calls, "sendMessage", "text"); text != emptyWeekPostText ||
		strings.Contains(lastParam(calls, "sendMessage", "reply_markup"), `"callback_data":"`+submitPrefix) {
		t.Fatalf("a post with just a date can be submitted: %+v", calls)
	}

	location := b.cfg.Location
	notifier := notify.New(b.store, b.cal, b, location, b.userID, b.cfg.DefaultIssue)
	beforeReminder := time.Date(2026, 11, 18, 18, 59, 0, 0, location)
	if err := notifier.Tick(ctx, beforeReminder); err != nil {
		t.Fatal(err)
	}
	if calls := tg.take(); len(calls) != 0 {
		t.Fatalf("the reminder was sent early: %+v", calls)
	}
	for _, offset := range []time.Duration{time.Minute, 2 * time.Minute} {
		if err := notifier.Tick(ctx, beforeReminder.Add(offset)); err != nil {
			t.Fatal(err)
		}
	}
	calls = tg.take()
	if text := lastParam(calls, "sendMessage", "text"); countMethod(calls, "sendMessage") != 1 ||
		!strings.Contains(text, "отсутствуют записи за сегодня (ср 18.11)") {
		t.Fatalf("expected one daily reminder for the empty post: %+v", calls)
	}
	if markup := lastParam(calls, "sendMessage", "reply_markup"); !strings.Contains(markup, postURL(next.Chat.ID, int64(next.ID))) {
		t.Fatalf("the reminder does not link to the post: %s", markup)
	}
}

func TestMissingCurrentWeekPost(t *testing.T) {
	b, tg, runner, st := newTelegramBot(t)
	ctx := context.Background()
	old := channelPost("23.10\nРабота - 8ч", day(10, 23))
	old.Chat.ID = -1001234567890
	b.cfg.ChannelID = old.Chat.ID
	b.onChannelPost(ctx, old, false)
	b.onMessage(ctx, dm("/status"))
	previousID := liveID(b)
	tg.take()
	// После перезапуска бот помнит старый предпросмотр
	restarted, err := New(b.cfg, st, b.cal, runner, b.userID)
	if err != nil {
		t.Fatal(err)
	}
	b = restarted
	b.clock = func() time.Time { return day(10, 26) }
	tg.take()
	tg.mu.Lock()
	tg.deletesFail = true
	tg.mu.Unlock()
	if err := b.DailyReminder(ctx, b.today()); err != nil {
		t.Fatal(err)
	}
	calls := tg.take()
	if text := lastParam(calls, "sendMessage", "text"); text != "📝 "+noWeekPostText {
		t.Fatalf("reminder = %q, want a missing current week post", text)
	}
	if markup := lastParam(calls, "sendMessage", "reply_markup"); !strings.Contains(markup, "В канал") ||
		!strings.Contains(markup, fmt.Sprintf("https://t.me/c/1234567890/%d", old.ID)) || countMethod(calls, "getChat") != 0 {
		t.Fatalf("reminder does not link to the last known post: %+v", calls)
	}
	if lastParam(calls, "deleteMessage", "message_id") != previousID ||
		lastParam(calls, "editMessageText", "message_id") != previousID ||
		!strings.Contains(lastParam(calls, "editMessageText", "text"), "прошлый предпросмотр") ||
		lastParam(calls, "editMessageText", "reply_markup") != `{"inline_keyboard":[]}` {
		t.Fatalf("old preview was not compacted: %+v", calls)
	}

	waitingID := liveID(b)
	next := channelPost("26.10\nРабота - 8ч", day(10, 26))
	next.Chat.ID = old.Chat.ID
	b.onChannelPost(ctx, next, false)
	calls = tg.take()
	if !inPlace(calls, waitingID, "Неделя 26.10") || liveID(b) != waitingID {
		t.Fatalf("waiting message was not converted to the new preview: %+v", calls)
	}
	if markup := lastParam(calls, "editMessageText", "reply_markup"); strings.Contains(markup, "В канал") ||
		!strings.Contains(markup, fmt.Sprintf("https://t.me/c/1234567890/%d", next.ID)) {
		t.Fatalf("preview does not link to the current post: %s", markup)
	}
}

func TestMissingWeekPostLinksToConfiguredChannel(t *testing.T) {
	b, tg, _, _ := newTelegramBot(t)
	b.cfg.ChannelID = -1001234567890
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		info models.ChatFullInfo
		url  string
	}{
		{"public", models.ChatFullInfo{Username: "example_channel"}, "https://t.me/example_channel"},
		{"private invite", models.ChatFullInfo{InviteLink: "https://t.me/+example"}, "https://t.me/+example"},
		{"private pinned", models.ChatFullInfo{PinnedMessage: &models.Message{ID: 123}}, "https://t.me/c/1234567890/123"},
		{"no available link", models.ChatFullInfo{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tg.mu.Lock()
			tc.info.ID = b.cfg.ChannelID
			tg.chatInfo = tc.info
			tg.mu.Unlock()
			b.onMessage(ctx, dm("/status"))
			calls := tg.take()
			if got := lastParam(calls, "getChat", "chat_id"); got != fmt.Sprint(b.cfg.ChannelID) {
				t.Fatalf("getChat for %q, want the configured channel", got)
			}
			markup := lastParam(calls, "sendMessage", "reply_markup")
			if tc.url == "" {
				if strings.Contains(markup, "В канал") {
					t.Fatalf("unexpected channel button: %s", markup)
				}
			} else if !strings.Contains(markup, "В канал") || !strings.Contains(markup, tc.url) {
				t.Fatalf("missing channel link %q: %s", tc.url, markup)
			}
		})
	}
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
	b.onMessage(ctx, dm("/submit"))
	saved, err := st.LatestWeekPost(ctx, b.userID)
	if err != nil {
		t.Fatal(err)
	}
	b.pending = &pendingSubmit{postID: saved.ID, hash: hashText(saved.Text), promptID: 7, expires: fixedClock().Add(time.Minute)}
	tg.take()

	b.onCallback(ctx, &models.CallbackQuery{
		ID: "cb", From: models.User{ID: b.cfg.OwnerID}, Data: fmt.Sprintf("%s%d:%s:0", submitPrefix, saved.ID, hashText(saved.Text)),
		Message: models.MaybeInaccessibleMessage{Message: &models.Message{ID: b.active.MessageID}},
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

func TestWidgetProbe(t *testing.T) {
	b, tg, _, _ := newTelegramBot(t)
	ctx := context.Background()
	pending := &pendingSubmit{promptID: 7, expires: fixedClock().Add(-time.Minute)}
	b.pending = pending
	cmd := dm("/widgettest")
	b.onMessage(ctx, cmd)
	calls := tg.take()
	if len(calls) != 2 || calls[0].method != "sendWidget" || calls[1].method != "deleteMessage" {
		t.Fatalf("widget probe calls = %+v", calls)
	}
	if calls[0].params["chat_id"] != strconv.FormatInt(b.cfg.OwnerID, 10) || calls[0].params["html"] != widgetProbeHTML {
		t.Fatalf("widget probe params = %+v", calls[0].params)
	}
	if b.pending != pending {
		t.Fatal("widget command consumed pending passphrase request")
	}

	tg.mu.Lock()
	tg.widgetDenied = true
	tg.mu.Unlock()
	b.onMessage(ctx, dm("/widgettest"))
	calls = tg.take()
	if countMethod(calls, "sendWidget") != 1 || !strings.Contains(lastParam(calls, "sendMessage", "text"), "400 Bad Request: INPUT_MEDIA_WIDGET_NOT_ALLOWED") {
		t.Fatalf("widget error was not reported: %+v", calls)
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

// Меню видно в личке владельца, после ответа удаляются только команды предпросмотра
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
		message := dm("/" + c.Command)
		b.onMessage(context.Background(), message)
		calls := tg.take()
		deleted := lastParam(calls, "deleteMessage", "message_id") == fmt.Sprint(message.ID)
		if want := c.Command == "status" || c.Command == "submit"; deleted != want {
			t.Errorf("/%s deleted = %v, want %v", c.Command, deleted, want)
		}
	}
}

// /status -> "К отправке" перерисовывает то же сообщение с кнопками записи, "Статус" присылает новое
func TestPreviewButtons(t *testing.T) {
	b, tg, _, _ := newTelegramBot(t)
	ctx := context.Background()
	b.onChannelPost(ctx, channelPost("19.10\nРабота - 8ч", day(10, 19)), false)
	tg.take()

	status := dm("/status")
	b.onMessage(ctx, status)
	calls := tg.take()
	if countMethod(calls, "sendRichMessage") != 1 || !strings.Contains(lastParam(calls, "sendRichMessage", "reply_markup"), viewSubmit) {
		t.Fatalf("/status has no way to submit: %+v", calls)
	}
	if got := lastParam(calls, "sendRichMessage", "text"); !strings.Contains(got, "<td>🔹&#8288;</td>") ||
		!strings.Contains(got, "<td>▫️&#8288;</td>") || !strings.Contains(got, "<td>▫️ сегодня</td>") {
		t.Fatalf("preview did not handle standalone emoji in its table:\n%s", got)
	}
	if calls[0].method != "sendRichMessage" || lastParam(calls, "deleteMessage", "message_id") != fmt.Sprint(status.ID) {
		t.Fatalf("/status was not deleted after the preview was sent: %+v", calls)
	}

	statusID := b.active.MessageID
	press := func(data string, messageID int) []apiCall {
		b.onCallback(ctx, &models.CallbackQuery{
			ID: "cb", From: models.User{ID: b.cfg.OwnerID}, Data: data,
			Message: models.MaybeInaccessibleMessage{Message: &models.Message{ID: messageID}},
		})
		return tg.take()
	}

	calls = press(viewSubmit, statusID)
	if countMethod(calls, "sendMessage") != 0 || countMethod(calls, "sendRichMessage") != 0 || lastParam(calls, "editMessageText", "message_id") != fmt.Sprint(statusID) ||
		!strings.Contains(lastParam(calls, "editMessageText", "reply_markup"), submitPrefix) {
		t.Fatalf("submit view did not replace the message: %+v", calls)
	}

	if err := b.WeeklyPreview(ctx); err != nil {
		t.Fatal(err)
	}
	calls = tg.take()
	fridayID := b.active.MessageID
	if lastParam(calls, "deleteMessage", "message_id") != fmt.Sprint(statusID) || fridayID == statusID {
		t.Fatalf("weekly preview did not replace the previous status: %+v", calls)
	}
	if calls := press(viewStatus, statusID); len(calls) != 1 ||
		lastParam(calls, "answerCallbackQuery", "text") != "Предпросмотр устарел" || b.active.MessageID != fridayID {
		t.Fatalf("stale refresh touched the weekly preview: %+v", calls)
	}
	if calls := press(viewSubmit, fridayID); !inPlace(calls, fmt.Sprint(fridayID), "Конец рабочей недели") ||
		!strings.Contains(lastParam(calls, "editMessageText", "reply_markup"), submitPrefix) {
		t.Fatalf("weekly preview lost its header or buttons on refresh: %+v", calls)
	}

	calls = press(viewNew, fridayID)
	if countMethod(calls, "editMessageText") != 0 || countMethod(calls, "sendRichMessage") != 1 {
		t.Fatalf("status button did not send a new preview: %+v", calls)
	}

	tg.mu.Lock()
	tg.sendsFail = true
	tg.mu.Unlock()
	b.onMessage(ctx, dm("/submit"))
	if calls := tg.take(); countMethod(calls, "sendRichMessage") != 1 || countMethod(calls, "deleteMessage") != 0 {
		t.Fatalf("the command was not kept after a failed send: %+v", calls)
	}
}

func lastParam(calls []apiCall, method, param string) string {
	v := ""
	for _, c := range calls {
		if c.method == method {
			if param == "text" {
				v = c.text()
				continue
			}
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
	if strings.Contains(preview, "К записи в Jira") || !strings.Contains(preview, "<tr><td>Пн 19.10</td><td>8ч</td><td>⚠️ вручную · в Jira 6ч</td></tr>") {
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
	live := liveID(b)

	b.pending = &pendingSubmit{postID: saved.ID, hash: hashText(saved.Text), promptID: 7, expires: fixedClock().Add(time.Minute)}
	b.onMessage(ctx, dm("Str0ng-Passphrase-XYZ"))
	calls := tg.take()
	// После отправки живой предпросмотр показывает свежее состояние на месте
	if lastParam(calls, "editMessageText", "message_id") != live || !strings.Contains(lastParam(calls, "editMessageText", "text"), "<tr><td>Вт 20.10</td><td>8ч</td><td>☑️&#8288;</td></tr>") {
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
	if preview := strings.Join(tg.sent(tg.take()), "\n"); !strings.Contains(preview, "<tr><td>Вт 20.10</td><td>8ч</td><td>☑️&#8288;</td></tr>") || strings.Contains(preview, "К записи") {
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

func TestBotButtonMovesToNewWeek(t *testing.T) {
	b, tg, runner, st := newTelegramBot(t)
	ctx := context.Background()
	old := bareChannelPost("23.10\nРабота - 8ч", day(10, 23))
	other := models.InlineKeyboardButton{Text: "Справка", URL: "https://example.com/help"}
	old.ReplyMarkup = &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{{other}}}
	b.onChannelPost(ctx, old, false)
	initial := lastParam(tg.take(), "editMessageReplyMarkup", "reply_markup")
	if !strings.Contains(initial, testBotURL) || !strings.Contains(initial, other.URL) {
		t.Fatalf("button was not added beside the existing keyboard: %s", initial)
	}

	restarted, err := New(b.cfg, st, b.cal, runner, b.userID)
	if err != nil {
		t.Fatal(err)
	}
	b = restarted
	b.clock = func() time.Time { return day(10, 26) }
	tg.take()

	next := bareChannelPost("26.10\nРабота - 8ч", day(10, 26))
	b.onChannelPost(ctx, next, false)
	var edited []apiCall
	for _, call := range tg.take() {
		if call.method == "editMessageReplyMarkup" {
			edited = append(edited, call)
		}
	}
	if len(edited) != 2 ||
		edited[0].params["message_id"] != fmt.Sprint(next.ID) || !strings.Contains(edited[0].params["reply_markup"], testBotURL) ||
		edited[1].params["message_id"] != fmt.Sprint(old.ID) || strings.Contains(edited[1].params["reply_markup"], testBotURL) ||
		!strings.Contains(edited[1].params["reply_markup"], other.URL) {
		t.Fatalf("button did not move while preserving the old keyboard: %+v", edited)
	}
	b.onChannelPost(ctx, edit(old, "23.10\nРабота - 7ч", day(10, 26)), true)
	if calls := tg.take(); countMethod(calls, "editMessageReplyMarkup") != 0 {
		t.Fatalf("editing the old post moved the button back: %+v", calls)
	}
}

func TestWeekArchiveButtonLifecycle(t *testing.T) {
	b, tg, runner, st := newTelegramBot(t)
	ctx := context.Background()
	b.cfg.ChannelID = -1001234567890
	runner.inJira = map[string]time.Duration{"2026-10-19": 8 * time.Hour}
	text := "19.10\nРевью кода - 8ч\n20.10\nРабота - 8ч\n21.10\nРабота - 8ч\n22.10\nРабота - 8ч\n23.10\nРабота - 8ч"
	old := bareChannelPost(text, day(10, 19))
	old.Chat.ID = b.cfg.ChannelID
	other := models.InlineKeyboardButton{Text: "Справка", URL: "https://example.com/help"}
	old.ReplyMarkup = &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{{other}}}
	b.onChannelPost(ctx, old, false)
	tg.take()
	post, err := st.WeekPostByMessage(ctx, old.Chat.ID, int64(old.ID))
	if err != nil {
		t.Fatal(err)
	}

	submit := func(approval bool) []apiCall {
		b.onMessage(ctx, dm("/submit"))
		tg.take()
		b.pending = &pendingSubmit{
			postID: post.ID, hash: hashText(text), approve: approval,
			promptID: 7, expires: b.clock().Add(time.Minute),
		}
		b.onMessage(ctx, dm("test-passphrase"))
		return tg.take()
	}
	if calls := submit(false); countMethod(calls, "editMessageReplyMarkup") != 0 ||
		!strings.Contains(lastParam(calls, "editMessageText", "text"), "можно отправить на проверку") {
		t.Fatalf("recording worklogs prematurely archived the week: %+v", calls)
	}

	payload := archiveStartPrefix + strconv.FormatInt(post.ID, 10)
	archiveLink := testBotURL + "?start=" + payload
	calls := submit(true)
	markup := lastParam(calls, "editMessageReplyMarkup", "reply_markup")
	if lastParam(calls, "editMessageReplyMarkup", "message_id") != fmt.Sprint(old.ID) ||
		!strings.Contains(markup, archiveButtonText) || !strings.Contains(markup, archiveLink) ||
		!strings.Contains(markup, other.URL) || strings.Contains(markup, botButtonText) {
		t.Fatalf("approval did not replace only the bot button: %+v", calls)
	}

	// Архив открывается даже во время ожидания фразы и не меняет живой предпросмотр
	active := liveID(b)
	pending := &pendingSubmit{expires: b.clock().Add(time.Minute)}
	b.pending = pending
	b.onMessage(ctx, dm("/start "+payload))
	calls = tg.take()
	if result := lastParam(calls, "sendRichMessage", "text"); !strings.Contains(result, "<blockquote><table compact><tr><td>Пн 19.10</td><td>8ч</td><td>☑️ вручную</td></tr>") ||
		!strings.Contains(result, "Всё записано и отправлено на проверку") || !strings.Contains(result, "вручную") ||
		strings.Contains(result, "сегодня") || b.pending != pending || liveID(b) != active {
		t.Fatalf("incorrect archived result or changed bot state: %+v", calls)
	}
	if markup := lastParam(calls, "sendRichMessage", "reply_markup"); !strings.Contains(markup, postURL(old.Chat.ID, int64(old.ID))) ||
		!strings.Contains(markup, "Скрыть") || strings.Contains(markup, "Обновить") {
		t.Fatalf("archive has incorrect controls: %s", markup)
	}
	b.pending = nil

	changed := edit(old, strings.Replace(text, "23.10\nРабота - 8ч", "23.10\nРабота - 7ч", 1), day(10, 23))
	changed.ReplyMarkup = &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{
		{other}, {{Text: archiveButtonText, URL: archiveLink}},
	}}
	b.onChannelPost(ctx, changed, true)
	calls = tg.take()
	if markup := lastParam(calls, "editMessageReplyMarkup", "reply_markup"); !strings.Contains(markup, botButtonText) ||
		!strings.Contains(markup, other.URL) || strings.Contains(markup, archiveLink) {
		t.Fatalf("mismatch did not restore the working button: %+v", calls)
	}
	b.onMessage(ctx, dm("/start "+payload))
	if calls := tg.take(); lastParam(calls, "sendMessage", "text") != "Итог недели недоступен" {
		t.Fatalf("the old archive is available after a mismatch: %+v", calls)
	}

	restored := edit(old, text, day(10, 23).Add(time.Hour))
	restored.ReplyMarkup = &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{
		{other}, {{Text: botButtonText, URL: testBotURL}},
	}}
	b.onChannelPost(ctx, restored, true)
	if calls := tg.take(); !strings.Contains(lastParam(calls, "editMessageReplyMarkup", "reply_markup"), archiveLink) {
		t.Fatalf("matching worklogs did not restore the archive: %+v", calls)
	}

	b.clock = func() time.Time { return day(10, 26) }
	next := bareChannelPost("26.10", day(10, 26))
	next.Chat.ID = old.Chat.ID
	b.onChannelPost(ctx, next, false)
	calls = tg.take()
	if countMethod(calls, "editMessageReplyMarkup") != 1 ||
		lastParam(calls, "editMessageReplyMarkup", "message_id") != fmt.Sprint(next.ID) {
		t.Fatalf("starting the new week changed the archived button: %+v", calls)
	}
	b.onChannelPost(ctx, edit(old, "19.10\nРабота", day(10, 26)), true)
	if calls := tg.take(); len(calls) != 0 {
		t.Fatalf("the archived post edit triggered Telegram calls: %+v", calls)
	}
	b.onMessage(ctx, dm("/start "+payload))
	if result := lastParam(tg.take(), "sendRichMessage", "text"); !strings.Contains(result, "Неделя 19.10") ||
		!strings.Contains(result, "Всё записано и отправлено на проверку") {
		t.Fatalf("the archived week was lost after rollover: %q", result)
	}
}

func TestWeekArchiveReplacesPreviousCard(t *testing.T) {
	b, tg, runner, st := newTelegramBot(t)
	ctx := context.Background()
	saveArchive := func(messageID int64, archiveText string) string {
		t.Helper()
		post, _, err := st.SaveWeekPost(ctx, store.WeekPost{
			UserID: b.userID, ChatID: b.cfg.ChannelID, MessageID: messageID, PostedAt: day(10, 12),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.SaveWeekArchive(ctx, post.ID, archiveText); err != nil {
			t.Fatal(err)
		}
		return archiveStartPrefix + strconv.FormatInt(post.ID, 10)
	}
	const header = "<b>Неделя 12.10-16.10.2026</b>\n"
	const summary = "\n<b>Итого: 14ч</b>"
	const savedText = header + "<pre>Пн 12.10  8ч        ☑️ вручную\nВт 13.10  6ч из 8ч  ☑️\n    ABC-1 4ч, ABC-2 2ч\n</pre>" + summary
	const savedQuote = header + "<blockquote><code>Пн 12.10 8ч ☑️ вручную\nВт 13.10 6ч ☑️ норма 8ч\n    ABC-1 4ч, ABC-2 2ч</code></blockquote>" + summary
	const displayedText = `<p><b>Неделя 12.10-16.10.2026</b></p><blockquote><table compact>` +
		`<tr><td>Пн 12.10</td><td>8ч</td><td>☑️ вручную</td></tr><tr><td>Вт 13.10</td><td>6ч</td><td>☑️ норма 8ч</td></tr>` +
		`<tr><td colspan="3">↳ ABC-1 4ч, ABC-2 2ч</td></tr></table></blockquote><p><b>Итого: 14ч</b></p>`
	first, other := saveArchive(42, savedText), saveArchive(43, savedQuote)
	b.onMessage(ctx, dm("/status"))
	active := liveID(b)
	tg.take()

	b.onMessage(ctx, dm("/start "+first))
	if got := lastParam(tg.take(), "sendRichMessage", "text"); got != displayedText {
		t.Fatalf("old archive was not reformatted:\n%s", got)
	}
	post, err := st.WeekPostByMessage(ctx, b.cfg.ChannelID, 42)
	if err != nil {
		t.Fatal(err)
	}
	old, err := st.WeekArchive(ctx, b.userID, b.cfg.ChannelID, post.ID)
	if err != nil || old.MessageID == 0 || old.Text != savedText {
		t.Fatalf("archive card was not saved: %+v, %v", old, err)
	}
	b.onMessage(ctx, dm("/start "+other))
	if calls := tg.take(); countMethod(calls, "deleteMessage") != 1 || lastParam(calls, "sendRichMessage", "text") != displayedText {
		t.Fatalf("opening another archive changed the formatting or touched the previous card: %+v", calls)
	}
	const savedTable = header + `<blockquote><table compact><tr><td>Пн 12.10</td><td>8ч</td><td>☑️</td></tr></table></blockquote>` + summary
	b.onMessage(ctx, dm("/start "+saveArchive(44, savedTable)))
	if got := lastParam(tg.take(), "sendRichMessage", "text"); !strings.Contains(got, "<td>☑️&#8288;</td>") {
		t.Fatalf("emoji in the saved table was not made compact:\n%s", got)
	}

	restarted, err := New(b.cfg, st, b.cal, runner, b.userID)
	if err != nil {
		t.Fatal(err)
	}
	b = restarted
	tg.take()
	b.onMessage(ctx, dm("/start "+first))
	calls := tg.take()
	if countMethod(calls, "sendRichMessage") != 1 || countMethod(calls, "deleteMessage") != 2 || liveID(b) != active ||
		!slices.ContainsFunc(calls, func(c apiCall) bool {
			return c.method == "deleteMessage" && c.params["message_id"] == fmt.Sprint(old.MessageID)
		}) {
		t.Fatalf("opening the same archive did not replace its card: %+v", calls)
	}

	current, err := st.WeekArchive(ctx, b.userID, b.cfg.ChannelID, post.ID)
	if err != nil || current.MessageID == old.MessageID {
		t.Fatalf("replacement card was not saved: %+v, %v", current, err)
	}
	tg.mu.Lock()
	tg.deletesFail = true
	tg.mu.Unlock()
	b.onMessage(ctx, dm("/start "+first))
	calls = tg.take()
	if lastParam(calls, "editMessageText", "message_id") != fmt.Sprint(current.MessageID) ||
		lastParam(calls, "editMessageText", "text") != "Итог недели обновлён" ||
		lastParam(calls, "editMessageText", "reply_markup") != `{"inline_keyboard":[]}` {
		t.Fatalf("the previous archive was not compacted after failed deletion: %+v", calls)
	}
}
