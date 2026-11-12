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
	ProxyURL secret.String
}

const (
	passphraseTimeout = 2 * time.Minute
	// Не больше стольких попыток отправки за час: каждая попытка - пуш MFA и логин в домен
	maxAttemptsPerHour = 3
)

type Bot struct {
	// clock подменяется в тестах
	clock  func() time.Time
	api    *tgbot.Bot
	cfg    Config
	store  *store.Store
	cal    Calendar
	runner Runner
	userID int64

	// Апдейты канала обрабатываются по одному
	postMu sync.Mutex
	// Уведомления о проблемах в постах по ID поста, под postMu. Только в памяти: после перезапуска
	// старое уведомление останется висеть, а новое придёт отдельно
	notices map[int64]postNotice
	// Идущая отправка: при остановке её нужно дождаться, чтобы туннель погас
	inflight sync.WaitGroup

	mu       sync.Mutex
	pending  *pendingSubmit
	running  bool
	locked   bool
	attempts []time.Time
	// Когда последний раз предупреждали, что токеном бота пользуется кто-то ещё
	conflictAlertAt time.Time
}

func New(cfg Config, st *store.Store, cal Calendar, runner Runner, userID int64) (*Bot, error) {
	b := &Bot{clock: time.Now, cfg: cfg, store: st, cal: cal, runner: runner, userID: userID}
	opts := []tgbot.Option{
		tgbot.WithDefaultHandler(b.handle),
		tgbot.WithErrorsHandler(b.onError),
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
		// Таймауты как у клиента библиотеки по умолчанию: long polling держит запрос до минуты
		opts = append(opts, tgbot.WithHTTPClient(time.Minute, &http.Client{Timeout: time.Minute, Transport: tr}))
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

func (b *Bot) edit(ctx context.Context, messageID int, text string, markup models.ReplyMarkup) {
	_, err := b.api.EditMessageText(ctx, &tgbot.EditMessageTextParams{
		ChatID:             b.cfg.OwnerID,
		MessageID:          messageID,
		Text:               text,
		ParseMode:          models.ParseModeHTML,
		LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: tgbot.True()},
		ReplyMarkup:        markup,
	})
	if err != nil && !strings.Contains(err.Error(), "message is not modified") {
		slog.Error("edit message", "error", b.redact(err.Error()))
	}
}

func (b *Bot) removeKeyboard(ctx context.Context, messageID int) {
	_, err := b.api.EditMessageReplyMarkup(ctx, &tgbot.EditMessageReplyMarkupParams{
		ChatID: b.cfg.OwnerID, MessageID: messageID,
	})
	if err != nil {
		slog.Error("remove keyboard", "error", b.redact(err.Error()))
	}
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

// onChannelPost следит за постом недели. Правки уже известного поста принимаются всегда. Новое сообщение
// становится постом недели, только если разбирается без ошибок и относится к более поздней неделе, чем текущий
// пост: так заметки в канале его не перехватят. Остальные сообщения молча игнорируются и в базу не попадают
func (b *Bot) onChannelPost(ctx context.Context, msg *models.Message, edited bool) {
	if msg.Chat.ID != b.cfg.ChannelID || !parser.LooksLikePost(msg.Text) {
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

	_, err := b.store.WeekPostByMessage(ctx, post.ChatID, post.MessageID)
	switch {
	case err == nil:
		b.updateKnownPost(ctx, post)
	case errors.Is(err, store.ErrNotFound):
		b.considerNewPost(ctx, post)
	default:
		slog.Error("find week post", "error", err)
	}
}

// updateKnownPost сохраняет правку поста недели и обновляет уведомление о проблемах в нём
func (b *Bot) updateKnownPost(ctx context.Context, post store.WeekPost) {
	saved, updated, err := b.store.SaveWeekPost(ctx, post)
	if err != nil {
		slog.Error("save week post", "error", err)
		b.notify(ctx, "⚠️ Правка поста недели не сохранена, подробности в логах бота")
		return
	}
	if !updated {
		// Пришла версия старше сохранённой
		return
	}

	b.showNotice(ctx, saved, b.noticeText(ctx, saved))
}

// postNotice - уведомление о проблемах в посте: у каждого поста не больше одного
type postNotice struct {
	messageID int
	text      string
}

// noticeText - всё, что сейчас не так с постом: ошибки разбора или предупреждения. Пусто - всё в порядке
func (b *Bot) noticeText(ctx context.Context, post store.WeekPost) string {
	report, err := parser.Parse(post.Text, b.cfg.DefaultIssue, b.now())
	if err != nil {
		return "⚠️ В посте недели ошибки:\n" + formatParseErrors(err)
	}
	var parts []string
	if warning := emptyDatesWarning(report); warning != "" {
		parts = append(parts, warning)
	}
	if warning, err := b.changedSubmitted(ctx, report); err != nil {
		slog.Error("compare with submitted", "error", err)
	} else if warning != "" {
		parts = append(parts, warning)
	}
	return strings.Join(parts, "\n\n")
}

// showNotice держит у поста одно уведомление. Текст не изменился - бот молчит. Изменился - старое
// удаляется и приходит новое, внизу чата и с пушем. Всё исправлено - старое просто удаляется
func (b *Bot) showNotice(ctx context.Context, post store.WeekPost, text string) {
	old := b.notices[post.ID]
	if text == old.text {
		return
	}
	if old.messageID != 0 {
		if err := b.deleteMessage(ctx, b.cfg.OwnerID, old.messageID); err != nil {
			slog.Warn("delete post notice", "error", err)
		}
	}
	delete(b.notices, post.ID)
	if text == "" {
		return
	}
	// "Обновить" перерисует уведомление в статус недели, дальше бот его не трогает
	msg, err := b.send(ctx, text, previewKeyboard(previewKeys{PostURL: postURL(post.ChatID, post.MessageID)}))
	if err != nil {
		slog.Error("send post notice", "error", err)
		return
	}
	if b.notices == nil {
		b.notices = map[int64]postNotice{}
	}
	b.notices[post.ID] = postNotice{messageID: msg.ID, text: text}
}

// forgetNotice - уведомление стало статусом недели по кнопке "Обновить": удалять его больше нельзя
func (b *Bot) forgetNotice(messageID int) {
	b.postMu.Lock()
	defer b.postMu.Unlock()
	for id, n := range b.notices {
		if n.messageID == messageID {
			delete(b.notices, id)
		}
	}
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

func (b *Bot) considerNewPost(ctx context.Context, post store.WeekPost) {
	report, err := parser.Parse(post.Text, b.cfg.DefaultIssue, b.now())
	if err != nil {
		return
	}
	current, err := b.store.LatestWeekPost(ctx, b.userID)
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		slog.Error("load week post", "error", err)
		return
	default:
		if !weekStart(report.Dates()[0]).After(b.lastWeekOf(current)) {
			return
		}
	}
	if _, _, err := b.store.SaveWeekPost(ctx, post); err != nil {
		slog.Error("save week post", "error", err)
		b.notify(ctx, "⚠️ Пост недели не сохранён, подробности в логах бота")
	}
}

// lastWeekOf - понедельник последней недели в посте; если пост не разбирается, неделя его публикации
func (b *Bot) lastWeekOf(post store.WeekPost) time.Time {
	report, err := parser.Parse(post.Text, b.cfg.DefaultIssue, b.now())
	if err != nil {
		p := post.PostedAt.In(b.cfg.Location)
		return weekStart(time.Date(p.Year(), p.Month(), p.Day(), 0, 0, 0, 0, b.cfg.Location))
	}
	dates := report.Dates()
	return weekStart(dates[len(dates)-1])
}

// changedSubmitted предупреждает о правках в днях, которые уже записаны в Jira: бот их не перезаписывает
func (b *Bot) changedSubmitted(ctx context.Context, report parser.Report) (string, error) {
	// Берём целые недели: удалённый крайний день тоже должен попасть в проверку
	dates := report.Dates()
	from, to := weekStart(dates[0]), weekStart(dates[len(dates)-1]).AddDate(0, 0, 6)
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
	if !b.fromOwner(msg) {
		return
	}
	if b.takePassphrase(ctx, msg) {
		return
	}

	text := strings.TrimSpace(msg.Text)
	if !strings.HasPrefix(text, "/") {
		// Обычный текст боту не нужен. Если это опоздавшая пароль-фраза, ей нельзя висеть в чате
		if err := b.deleteMessage(ctx, msg.Chat.ID, msg.ID); err != nil {
			slog.Error("delete stray message", "error", err)
		}
		b.notify(ctx, "🧹 Сообщение удалено: бот понимает только команды и кнопки. "+
			"Если это была пароль-фраза - нужна повторная отправка\n/help - список команд")
		return
	}

	cmd, args, _ := strings.Cut(text, " ")
	cmd, _, _ = strings.Cut(cmd, "@")
	args = strings.TrimSpace(args)
	switch cmd {
	case "/start", "/help":
		b.notify(ctx, helpText)
	case "/status":
		b.sendPreview(ctx, false)
	case "/submit":
		b.sendPreview(ctx, true)
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
	case cq.Data == viewStatus || cq.Data == viewSubmit:
		b.answer(ctx, cq, "Обновлено", false)
		text, markup := b.buildPreview(ctx, cq.Data == viewSubmit)
		if messageID == 0 {
			b.sendPreview(ctx, cq.Data == viewSubmit)
			return
		}
		b.forgetNotice(messageID)
		b.edit(ctx, messageID, text, markup)
	case cq.Data == viewNew:
		b.answer(ctx, cq, "", false)
		b.sendPreview(ctx, false)
	case cq.Data == hideData:
		b.answer(ctx, cq, "", false)
		if messageID != 0 {
			if err := b.deleteMessage(ctx, b.cfg.OwnerID, messageID); err != nil {
				slog.Error("hide message", "error", err)
			}
		}
	default:
		b.answer(ctx, cq, "Кнопка устарела", false)
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
