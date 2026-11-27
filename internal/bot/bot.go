// Package bot - Telegram-интерфейс: читает пост недели из канала, показывает превью, принимает
// пароль-фразу и запускает отправку. Общается только с владельцем, всех остальных молча игнорирует
package bot

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/devWaylander/auto-time-report/internal/calendar"
	"github.com/devWaylander/auto-time-report/internal/config"
	"github.com/devWaylander/auto-time-report/internal/corp"
	"github.com/devWaylander/auto-time-report/internal/parser"
	"github.com/devWaylander/auto-time-report/internal/secret"
	"github.com/devWaylander/auto-time-report/internal/store"
)

// Runner - корпоративная часть: расшифровка доступов, VPN, Jira. Сейчас это corp.Runner в том же процессе,
// но бот завязан только на интерфейс, чтобы её можно было вынести на другую машину
type Runner interface {
	Run(ctx context.Context, passphrase []byte, job corp.Job, hooks corp.Hooks) (corp.Outcome, error)
}

type Calendar interface {
	Day(ctx context.Context, date time.Time) (calendar.DayKind, error)
}

type Config struct {
	Token        secret.String
	OwnerID      int64
	ChannelID    int64
	DefaultIssue string
	Location     *time.Location
	// Адрес Bot API; пусто - api.telegram.org. Нужен тестам и для своего Bot API сервера
	ServerURL string
	// Прокси для запросов к Bot API; пусто - напрямую
	ProxyURL   secret.String
	MiniAppURL *url.URL
}

const (
	passphraseTimeout = 2 * time.Minute
	// Не больше стольких попыток отправки за час: каждая попытка - пуш MFA и логин в домен
	maxAttemptsPerHour = 3
)

type Bot struct {
	// clock подменяется в тестах
	clock      func() time.Time
	api        *tgbot.Bot
	httpClient *http.Client
	cfg        Config
	store      *store.Store
	cal        Calendar
	runner     Runner
	userID     int64

	// Апдейты канала обрабатываются по одному
	postMu sync.Mutex
	// Текущий предпросмотр, под postMu
	active store.ActiveWeekView
	// Идущая отправка: при остановке её нужно дождаться, чтобы туннель погас
	inflight sync.WaitGroup

	mu       sync.Mutex
	pending  *pendingSubmit
	running  bool
	locked   bool
	attempts []time.Time
	// Когда последний раз предупреждали, что токеном бота пользуется кто-то ещё
	conflictAlertAt time.Time
	// Ссылка на чат с ботом для кнопки под постом; спрашивается у Telegram один раз
	botURL string
	// Telegram не дал повесить кнопку под постом: до перезапуска больше не пытаемся
	buttonOff bool
}

func New(cfg Config, st *store.Store, cal Calendar, runner Runner, userID int64) (*Bot, error) {
	active, err := st.ActiveWeekView(context.Background(), userID)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: time.Minute}
	b := &Bot{clock: time.Now, httpClient: client, cfg: cfg, store: st, cal: cal, runner: runner, userID: userID, active: active}
	opts := []tgbot.Option{
		tgbot.WithDefaultHandler(b.handle),
		tgbot.WithErrorsHandler(b.onError),
		tgbot.WithHTTPClient(time.Minute, client),
		tgbot.WithAllowedUpdates(tgbot.AllowedUpdates{
			"message", "channel_post", "edited_channel_post", "callback_query",
		}),
	}
	if cfg.ServerURL != "" {
		opts = append(opts, tgbot.WithServerURL(cfg.ServerURL))
	}
	if cfg.ProxyURL != "" {
		proxy, err := config.ParseProxyURL(cfg.ProxyURL.Reveal())
		if err != nil {
			return nil, fmt.Errorf("telegram proxy: %w", err)
		}
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.Proxy = http.ProxyURL(proxy)
		client.Transport = tr
	}
	api, err := tgbot.New(cfg.Token.Reveal(), opts...)
	if err != nil {
		return nil, errors.New(b.redact(fmt.Sprintf("telegram: %v", err)))
	}
	b.api = api
	return b, nil
}

// Start слушает Telegram, пока не отменят контекст
func (b *Bot) Start(ctx context.Context) {
	b.setCommands(ctx)
	b.api.Start(ctx)
}

// Wait ждёт окончания идущей отправки: при остановке она гасит туннель
func (b *Bot) Wait() {
	b.inflight.Wait()
}

// Ping проверяет связь с Telegram
func (b *Bot) Ping(ctx context.Context) error {
	if _, err := b.api.GetMe(ctx); err != nil {
		return errors.New(b.redact(err.Error()))
	}
	return nil
}

func (b *Bot) handle(ctx context.Context, _ *tgbot.Bot, u *models.Update) {
	// Апдейты не логируем: в них может быть пароль-фраза и содержимое постов
	switch {
	case u.ChannelPost != nil:
		b.onChannelPost(ctx, u.ChannelPost, false)
	case u.EditedChannelPost != nil:
		b.onChannelPost(ctx, u.EditedChannelPost, true)
	case u.CallbackQuery != nil:
		b.onCallback(ctx, u.CallbackQuery)
	case u.Message != nil:
		b.onMessage(ctx, u.Message)
	}
}

// fromOwner - личное сообщение от владельца; всё остальное бот не видит
func (b *Bot) fromOwner(msg *models.Message) bool {
	return msg.From != nil && msg.From.ID == b.cfg.OwnerID && msg.Chat.ID == b.cfg.OwnerID
}

func (b *Bot) onError(err error) {
	msg := b.redact(err.Error())
	if strings.Contains(msg, "decode update") {
		// Библиотека кладёт в эту ошибку сырой апдейт, а в нём может быть пароль-фраза
		msg = "failed to decode update"
	}
	slog.Error("telegram", "error", msg)

	// 409 Conflict: getUpdates вызывает ещё кто-то с тем же токеном
	if !errors.Is(err, tgbot.ErrorConflict) && !strings.Contains(strings.ToLower(msg), "conflict") {
		return
	}
	b.mu.Lock()
	if time.Since(b.conflictAlertAt) < time.Hour {
		b.mu.Unlock()
		return
	}
	b.conflictAlertAt = time.Now()
	b.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	b.notify(ctx, "🚨 Токеном бота пользуется ещё кто-то (Telegram вернул 409 Conflict)\n"+
		"Если это не второй запуск бота - токен перевыпустить в @BotFather, пароль-фразу до выяснения не отправлять")
}

// redact убирает токен бота из текста: библиотека кладёт URL с токеном в ошибки сети
func (b *Bot) redact(s string) string {
	if token := b.cfg.Token.Reveal(); token != "" {
		s = strings.ReplaceAll(s, token, "***")
	}
	if u, err := config.ParseProxyURL(b.cfg.ProxyURL.Reveal()); err == nil {
		if pass, ok := u.User.Password(); ok && pass != "" {
			s = strings.ReplaceAll(s, pass, "***")
		}
	}
	return s
}

func (b *Bot) now() time.Time {
	return b.clock().In(b.cfg.Location)
}

func (b *Bot) today() time.Time {
	now := b.now()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, b.cfg.Location)
}

// send пишет владельцу в личку, HTML-разметка
func (b *Bot) send(ctx context.Context, text string, markup models.ReplyMarkup) (*models.Message, error) {
	if rich := weekTableMessage(text, b.cfg.MiniAppURL != nil); rich != nil {
		msg, err := b.api.SendRichMessage(ctx, &tgbot.SendRichMessageParams{
			ChatID: b.cfg.OwnerID, RichMessage: *rich, ReplyMarkup: markup,
		})
		if err != nil {
			return nil, errors.New(b.redact(err.Error()))
		}
		return msg, nil
	}
	msg, err := b.api.SendMessage(ctx, &tgbot.SendMessageParams{
		ChatID:             b.cfg.OwnerID,
		Text:               text,
		ParseMode:          models.ParseModeHTML,
		LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: tgbot.True()},
		ReplyMarkup:        markup,
	})
	if err != nil {
		return nil, errors.New(b.redact(err.Error()))
	}
	return msg, nil
}

// notify - send без ответа: ошибку только логируем
func (b *Bot) notify(ctx context.Context, text string) {
	if _, err := b.send(ctx, text, nil); err != nil {
		slog.Error("send message", "error", err)
	}
}

// edit - editMessage без ответа: ошибку только логируем
func (b *Bot) edit(ctx context.Context, messageID int, text string, markup models.ReplyMarkup) {
	if err := b.editMessage(ctx, messageID, text, markup); err != nil {
		slog.Error("edit message", "error", err)
	}
}

// editMessage меняет текст сообщения в личке. Тот же текст - не ошибка
func (b *Bot) editMessage(ctx context.Context, messageID int, text string, markup models.ReplyMarkup) error {
	params := &tgbot.EditMessageTextParams{
		ChatID:             b.cfg.OwnerID,
		MessageID:          messageID,
		Text:               text,
		ParseMode:          models.ParseModeHTML,
		LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: tgbot.True()},
		ReplyMarkup:        markup,
	}
	if rich := weekTableMessage(text, b.cfg.MiniAppURL != nil); rich != nil {
		params.Text = ""
		params.ParseMode = ""
		params.LinkPreviewOptions = nil
		params.RichMessage = rich
	}
	_, err := b.api.EditMessageText(ctx, params)
	if err != nil && !strings.Contains(err.Error(), "message is not modified") {
		return errors.New(b.redact(err.Error()))
	}
	return nil
}

func (b *Bot) deleteMessage(ctx context.Context, chatID int64, messageID int) error {
	_, err := b.api.DeleteMessage(ctx, &tgbot.DeleteMessageParams{ChatID: chatID, MessageID: messageID})
	if err != nil {
		return errors.New(b.redact(err.Error()))
	}
	return nil
}

func (b *Bot) answer(ctx context.Context, cq *models.CallbackQuery, text string, alert bool) {
	_, err := b.api.AnswerCallbackQuery(ctx, &tgbot.AnswerCallbackQueryParams{
		CallbackQueryID: cq.ID, Text: text, ShowAlert: alert,
	})
	if err != nil {
		slog.Error("answer callback", "error", b.redact(err.Error()))
	}
}

// onChannelPost следит за постом текущей недели и принимает новый пост с её датой даже с ошибками
func (b *Bot) onChannelPost(ctx context.Context, msg *models.Message, edited bool) {
	if msg.Chat.ID != b.cfg.ChannelID || msg.Text == "" {
		return
	}
	// Хендлеры работают параллельно: апдейты канала разбираем по одному, чтобы решения не перемешались
	b.postMu.Lock()
	defer b.postMu.Unlock()

	post := store.WeekPost{
		UserID:    b.userID,
		ChatID:    msg.Chat.ID,
		MessageID: int64(msg.ID),
		Text:      msg.Text,
		PostedAt:  time.Unix(int64(msg.Date), 0),
	}
	if edited && msg.EditDate != 0 {
		post.EditedAt = time.Unix(int64(msg.EditDate), 0)
	}

	known, err := b.store.WeekPostByMessage(ctx, post.ChatID, post.MessageID)
	var saved store.WeekPost
	var updated bool
	switch {
	case err == nil:
		if !b.isCurrentWeek(known) {
			return
		}
		saved, updated = b.updateKnownPost(ctx, post)
	case errors.Is(err, store.ErrNotFound):
		if !parser.LooksLikePost(post.Text) {
			return
		}
		saved, updated = b.considerNewPost(ctx, post)
	default:
		slog.Error("find week post", "error", err)
	}
	if updated && b.isCurrentWeek(saved) {
		b.onPostChanged(ctx, saved, msg)
	}
}

// updateKnownPost сохраняет правку поста текущей недели
func (b *Bot) updateKnownPost(ctx context.Context, post store.WeekPost) (store.WeekPost, bool) {
	saved, updated, err := b.store.SaveWeekPost(ctx, post)
	if err != nil {
		slog.Error("save week post", "error", err)
		b.notify(ctx, "⚠️ Правка поста недели не сохранена, подробности в логах бота")
	}
	return saved, updated
}

// showPreview показывает текущую неделю, новый предпросмотр заменяет прежний
func (b *Bot) showPreview(ctx context.Context, submit bool, header string) error {
	b.postMu.Lock()
	defer b.postMu.Unlock()
	return b.showPreviewLocked(ctx, submit, 0, header)
}

func (b *Bot) showPreviewLocked(ctx context.Context, submit bool, messageID int, header string) error {
	week := calendar.WeekStart(b.now())
	w, err := b.loadWeek(ctx)
	if err == nil {
		b.removeResolvedDailyReminders(ctx, w.report, week)
	}
	v := b.viewOf(ctx, w, err, submit, header)
	b.syncWeekArchive(ctx, w.post, v, nil)
	if v.live || errors.Is(err, errNoPost) {
		return b.showActive(ctx, v, store.ActiveWeekView{
			MessageID: messageID, PostID: w.post.ID, WeekStart: week,
			Problems: v.problems, Submit: submit, Header: header,
		})
	}
	_, err = b.send(ctx, v.text, v.markup)
	return err
}

// showActive обновляет сообщение на месте или сначала отправляет новое, затем убирает прежнее
func (b *Bot) showActive(ctx context.Context, v weekView, next store.ActiveWeekView) error {
	if next.MessageID == 0 {
		msg, err := b.send(ctx, v.text, v.markup)
		if err != nil {
			return err
		}
		next.MessageID = msg.ID
	} else if err := b.editMessage(ctx, next.MessageID, v.text, v.markup); err != nil {
		if strings.Contains(err.Error(), "message to edit not found") {
			b.clearActive(ctx)
		}
		return err
	}
	old := b.active
	b.active = next
	if err := b.store.SaveActiveWeekView(context.WithoutCancel(ctx), b.userID, next); err != nil {
		slog.Error("save active preview", "error", err)
	}
	if old.MessageID != 0 && old.MessageID != next.MessageID {
		b.retireActive(ctx, old, next)
	}
	return nil
}

func (b *Bot) retireActive(ctx context.Context, old, next store.ActiveWeekView) {
	text := "Предпросмотр устарел"
	if old.WeekStart.Format(time.DateOnly) != next.WeekStart.Format(time.DateOnly) {
		text = fmt.Sprintf("Неделя с %s · прошлый предпросмотр", old.WeekStart.Format("02.01.2006"))
	}
	b.retireMessage(ctx, old.MessageID, text)
}

// retireMessage удаляет старое сообщение или сворачивает его до одной строки без кнопок
func (b *Bot) retireMessage(ctx context.Context, messageID int, text string) bool {
	if err := b.deleteMessage(ctx, b.cfg.OwnerID, messageID); err == nil || strings.Contains(err.Error(), "message to delete not found") {
		return true
	}
	noButtons := models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{}}
	if err := b.editMessage(ctx, messageID, text, noButtons); err != nil {
		slog.Warn("retire message", "error", err)
		return false
	}
	return true
}

func (b *Bot) clearActive(ctx context.Context) {
	b.active = store.ActiveWeekView{}
	if err := b.store.DeleteActiveWeekView(context.WithoutCancel(ctx), b.userID); err != nil {
		slog.Error("forget active preview", "error", err)
	}
}

// onPostChanged обновляет текущий предпросмотр и сообщает о новых проблемах, вызывать под postMu
func (b *Bot) onPostChanged(ctx context.Context, post store.WeekPost, msg *models.Message) {
	w, err := b.loadWeekOf(ctx, post)
	next := store.ActiveWeekView{PostID: post.ID, WeekStart: calendar.WeekStart(b.now())}
	if err == nil {
		b.removeResolvedDailyReminders(ctx, w.report, next.WeekStart)
	}
	active := b.active
	sameWeek := active.WeekStart.Format(time.DateOnly) == next.WeekStart.Format(time.DateOnly)
	if sameWeek {
		next.Submit, next.Header = active.Submit, active.Header
	}
	v := b.viewOf(ctx, w, err, next.Submit, next.Header)
	b.syncWeekArchive(ctx, post, v, msg)
	if !v.live || active.MessageID == 0 && v.problems == "" {
		return
	}
	next.Problems = v.problems
	// Новый пост без проблем обновляет текущий статус без пуша, даже при смене недели
	if v.problems == "" || sameWeek && (active.PostID == 0 && err == nil ||
		active.PostID == post.ID && v.problems == active.Problems) {
		next.MessageID = active.MessageID
	}
	if err := b.showActive(ctx, v, next); err != nil {
		slog.Warn("update active preview", "error", err)
	}
}

// refreshLive перерисовывает текущий предпросмотр после отправки в Jira
func (b *Bot) refreshLive(ctx context.Context, post store.WeekPost) {
	b.postMu.Lock()
	defer b.postMu.Unlock()
	current, err := b.store.WeekPostByMessage(ctx, post.ChatID, post.MessageID)
	if err != nil {
		slog.Warn("read post after submit", "error", err)
		return
	}
	if b.active.PostID == post.ID && b.isCurrentWeek(current) {
		b.onPostChanged(ctx, current, nil)
		return
	}
	w, err := b.loadWeekOf(ctx, current)
	b.syncWeekArchive(ctx, current, b.viewOf(ctx, w, err, false, ""), nil)
}

func (b *Bot) hideMessage(ctx context.Context, messageID int) bool {
	b.postMu.Lock()
	defer b.postMu.Unlock()
	if messageID == 0 || !b.retireMessage(ctx, messageID, "Сообщение скрыто") {
		return false
	}
	if b.active.MessageID == messageID {
		b.clearActive(ctx)
	}
	return true
}

func (b *Bot) isCurrentWeek(post store.WeekPost) bool {
	return b.postWeek(post) == calendar.WeekStart(b.now()).Format(time.DateOnly)
}

func (b *Bot) postWeek(post store.WeekPost) string {
	if post.WeekStart.IsZero() {
		return calendar.WeekStart(post.PostedAt.In(b.cfg.Location)).Format(time.DateOnly)
	}
	return post.WeekStart.Format(time.DateOnly)
}

// emptyDatesWarning - предупреждение о датах без записей. Это не ошибка: день может быть ещё не дописан
func emptyDatesWarning(report parser.Report) string {
	dates := make([]string, len(report.EmptyDates))
	for i, d := range report.EmptyDates {
		dates[i] = d.Format("02.01")
	}
	switch len(dates) {
	case 0:
		return ""
	case 1:
		return fmt.Sprintf("⚠️ %s: дата есть, записей под ней нет", dates[0])
	default:
		return fmt.Sprintf("⚠️ %s: даты есть, записей под ними нет", strings.Join(dates, ", "))
	}
}

// considerNewPost принимает новое сообщение как пост недели, если оно подходит; true - принято
func (b *Bot) considerNewPost(ctx context.Context, post store.WeekPost) (store.WeekPost, bool) {
	now := b.now()
	date := parser.FirstPostDate(post.Text, now)
	if date.IsZero() {
		return store.WeekPost{}, false
	}
	week := calendar.WeekStart(now)
	post.WeekStart = calendar.WeekStart(date)
	if !post.WeekStart.Equal(week) {
		return store.WeekPost{}, false
	}
	current, err := b.store.LatestWeekPost(ctx, b.userID)
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		slog.Error("load week post", "error", err)
		return store.WeekPost{}, false
	default:
		if b.postWeek(post) <= b.postWeek(current) {
			return store.WeekPost{}, false
		}
	}
	if _, err := b.store.WeekPostForWeek(ctx, b.userID, post.WeekStart); !errors.Is(err, store.ErrNotFound) {
		if err != nil {
			slog.Error("find post for week", "error", err)
		}
		return store.WeekPost{}, false
	}
	saved, _, err := b.store.SaveWeekPost(ctx, post)
	if err != nil {
		slog.Error("save week post", "error", err)
		b.notify(ctx, "⚠️ Пост недели не сохранён, подробности в логах бота")
		return store.WeekPost{}, false
	}
	return saved, true
}

// botLink - ссылка на чат с ботом. Имя бота спрашивается у Telegram один раз
func (b *Bot) botLink(ctx context.Context) (string, error) {
	b.mu.Lock()
	link := b.botURL
	b.mu.Unlock()
	if link != "" {
		return link, nil
	}
	me, err := b.api.GetMe(ctx)
	if err != nil {
		return "", errors.New(b.redact(err.Error()))
	}
	if me.Username == "" {
		return "", errors.New("telegram returned no bot username")
	}
	link = "https://t.me/" + me.Username
	b.mu.Lock()
	b.botURL = link
	b.mu.Unlock()
	return link, nil
}

// changedSubmitted предупреждает о правках в днях, которые уже записаны в Jira: бот их не перезаписывает
func (b *Bot) changedSubmitted(ctx context.Context, report parser.Report) (string, error) {
	// Берём целые недели: удалённый крайний день тоже должен попасть в проверку
	dates := report.Dates()
	from, to := calendar.WeekStart(dates[0]), calendar.WeekStart(dates[len(dates)-1]).AddDate(0, 0, 6)
	subs, err := b.store.SubmissionsBetween(ctx, b.userID, from, to)
	if err != nil {
		return "", err
	}
	var lines []string
	for _, s := range subs {
		var found *parser.Worklog
		for i, w := range report.Worklogs {
			if w.IssueKey == s.IssueKey && sameDay(w.Date, s.WorkDate) {
				found = &report.Worklogs[i]
				break
			}
		}
		where := "в Jira"
		if s.Manual {
			where = "в Jira вручную"
		}
		switch {
		case found == nil:
			lines = append(lines, fmt.Sprintf("• %s %s: %s %s, а из поста запись пропала",
				s.WorkDate.Format("02.01"), s.IssueKey, where, parser.FormatDuration(s.Duration)))
		case found.Duration() != s.Duration:
			lines = append(lines, fmt.Sprintf("• %s %s: %s %s, в посте теперь %s",
				s.WorkDate.Format("02.01"), s.IssueKey, where, parser.FormatDuration(s.Duration), parser.FormatDuration(found.Duration())))
		}
	}
	if len(lines) == 0 {
		return "", nil
	}
	return "⚠️ Изменены дни, уже записанные в Jira. Бот их не перезаписывает, правка вручную в календаре WorklogPRO:\n" +
		strings.Join(lines, "\n"), nil
}

func (b *Bot) onMessage(ctx context.Context, msg *models.Message) {
	if !b.fromOwner(msg) || msg.MessageAutoDeleteTimerChanged != nil {
		return
	}
	text := strings.TrimSpace(msg.Text)
	cmd, args, _ := strings.Cut(text, " ")
	cmd, _, _ = strings.Cut(cmd, "@")
	args = strings.TrimSpace(args)
	if id, archive := strings.CutPrefix(args, archiveStartPrefix); cmd == "/start" && archive {
		b.sendWeekArchive(ctx, msg, id)
		return
	}
	if cmd == "/widgettest" && args == "" {
		b.testWidget(ctx, msg)
		return
	}
	if b.takePassphrase(ctx, msg) {
		return
	}
	if !strings.HasPrefix(text, "/") {
		// Обычный текст боту не нужен. Если это опоздавшая пароль-фраза, ей нельзя висеть в чате
		if err := b.deleteMessage(ctx, msg.Chat.ID, msg.ID); err != nil {
			slog.Error("delete stray message", "error", err)
		}
		b.notify(ctx, "🧹 Сообщение удалено: бот понимает только команды и кнопки. "+
			"Если это была пароль-фраза - нужна повторная отправка\n/help - список команд")
		return
	}

	switch cmd {
	case "/start", "/help":
		b.notify(ctx, helpText)
	case "/status", "/submit":
		if b.sendPreview(ctx, cmd == "/submit") {
			if err := b.deleteMessage(ctx, msg.Chat.ID, msg.ID); err != nil {
				slog.Warn("delete preview command", "error", err)
			}
		}
	case "/settings":
		b.sendSettings(ctx)
	case "/daily":
		b.cmdDaily(ctx, args)
	case "/weekly":
		b.cmdWeekly(ctx, args)
	case "/vacation":
		b.cmdVacation(ctx, args)
	case "/cancel":
		b.cancelPending(ctx)
	case "/unlock":
		b.cmdUnlock(ctx)
	default:
		// Неизвестная "команда" может оказаться пароль-фразой, которая начинается со слэша
		if err := b.deleteMessage(ctx, msg.Chat.ID, msg.ID); err != nil {
			slog.Error("delete unknown command", "error", err)
		}
		b.notify(ctx, "🧹 Неизвестная команда, сообщение удалено\n/help - список команд")
	}
}

func (b *Bot) onCallback(ctx context.Context, cq *models.CallbackQuery) {
	if cq.From.ID != b.cfg.OwnerID {
		b.answer(ctx, cq, "Нет доступа", false)
		return
	}
	var messageID int
	if cq.Message.Message != nil {
		messageID = cq.Message.Message.ID
	}

	switch {
	case strings.HasPrefix(cq.Data, submitPrefix):
		b.onSubmitButton(ctx, cq, messageID)
	case strings.HasPrefix(cq.Data, settingsPrefix):
		b.onSettingsButton(ctx, cq, messageID)
	case cq.Data == viewStatus || cq.Data == viewSubmit || strings.HasPrefix(cq.Data, noticePrefix):
		b.onPreviewButton(ctx, cq, messageID)
	case cq.Data == viewNew:
		b.answer(ctx, cq, "", false)
		b.sendPreview(ctx, false)
	case cq.Data == hideData:
		if !b.hideMessage(ctx, messageID) {
			b.answer(ctx, cq, "Не удалось скрыть сообщение", false)
			return
		}
		b.answer(ctx, cq, "", false)
	default:
		b.answer(ctx, cq, "Кнопка устарела", false)
	}
}

func (b *Bot) onPreviewButton(ctx context.Context, cq *models.CallbackQuery, messageID int) {
	b.postMu.Lock()
	defer b.postMu.Unlock()
	if messageID == 0 || messageID != b.active.MessageID {
		b.answer(ctx, cq, "Предпросмотр устарел", false)
		return
	}
	header, submit := b.active.Header, cq.Data == viewSubmit
	if b.active.WeekStart.Format(time.DateOnly) != calendar.WeekStart(b.now()).Format(time.DateOnly) {
		header, submit = "", false
	}
	b.answer(ctx, cq, "Обновлено", false)
	if err := b.showPreviewLocked(ctx, submit, messageID, header); err != nil {
		slog.Error("refresh preview", "error", err)
	}
}

// command - команда бота. Из этого списка собираются /help и меню команд в Telegram, которое бот
// выставляет при каждом старте: новую команду достаточно добавить сюда и в switch выше
type command struct {
	name    string // без слэша
	example string // аргументы для примера в /help
	about   string // описание: в меню Telegram и в /help
	more    string // дополнение для /help
}

var commands = []command{
	{name: "status", about: "предпросмотр недели"},
	{name: "submit", about: "предпросмотр с кнопками отправки в Jira"},
	{name: "settings", about: "напоминания и пауза"},
	{name: "daily", example: "19:30", about: "время ежедневного напоминания"},
	{name: "weekly", example: "пт 16:00", about: "день и время недельного предпросмотра"},
	{name: "vacation", example: "27.10-31.10", about: "отпуск без напоминаний", more: ", /vacation off - отменить"},
	{name: "cancel", about: "отменить ожидание пароль-фразы"},
	{name: "unlock", about: "снять блокировку после ошибки доступа"},
	{name: "help", about: "список команд"},
}

var helpText = func() string {
	var b strings.Builder
	b.WriteString("<b>Команды</b>\n\n")
	b.WriteString("Пост недели ведётся в канале как обычно: бот видит каждую правку и пишет сюда об ошибках\n\n")
	for _, c := range commands {
		if c.name == "help" {
			continue
		}
		fmt.Fprintf(&b, "/%s", c.name)
		if c.example != "" {
			fmt.Fprintf(&b, " %s", c.example)
		}
		fmt.Fprintf(&b, " - %s%s\n", c.about, c.more)
	}
	b.WriteString("\nПароль-фраза - <b>только</b> в ответ на запрос бота")
	return b.String()
}()

// setCommands выставляет меню команд только в личке владельца: посторонний не увидит даже списка
func (b *Bot) setCommands(ctx context.Context) {
	menu := make([]models.BotCommand, len(commands))
	for i, c := range commands {
		desc := c.about
		if c.example != "" {
			desc += ", например /" + c.name + " " + c.example
		}
		menu[i] = models.BotCommand{Command: c.name, Description: desc}
	}
	_, err := b.api.SetMyCommands(ctx, &tgbot.SetMyCommandsParams{
		Commands: menu,
		Scope:    &models.BotCommandScopeChat{ChatID: b.cfg.OwnerID},
	})
	if err != nil {
		slog.Warn("set bot commands", "error", b.redact(err.Error()))
	}
}

// Сколько ошибок разбора показывать: сообщение Telegram ограничено 4096 символами
const maxShownErrors = 25

func formatParseErrors(err error) string {
	all := strings.Split(err.Error(), "\n")
	var lines []string
	for i, line := range all {
		if i == maxShownErrors {
			lines = append(lines, fmt.Sprintf("… и ещё %d", len(all)-maxShownErrors))
			break
		}
		lines = append(lines, "• "+html.EscapeString(line))
	}
	return strings.Join(lines, "\n")
}

func sameDay(a, b time.Time) bool {
	return a.Format("2006-01-02") == b.Format("2006-01-02")
}
