package bot

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"strconv"
	"strings"
	"time"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/devWaylander/auto-time-report/internal/corp"
	"github.com/devWaylander/auto-time-report/internal/jira"
	"github.com/devWaylander/auto-time-report/internal/parser"
	"github.com/devWaylander/auto-time-report/internal/store"
)

// pendingSubmit — нажата кнопка отправки, ждём пароль-фразу
type pendingSubmit struct {
	postID   int64
	hash     string
	approve  bool
	promptID int
	expires  time.Time
}

func parseSubmitData(data string) (postID int64, hash string, approve bool, err error) {
	parts := strings.Split(strings.TrimPrefix(data, submitPrefix), ":")
	if len(parts) != 3 || (parts[2] != "0" && parts[2] != "1") {
		return 0, "", false, fmt.Errorf("bad submit data %q", data)
	}
	postID, err = strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, "", false, fmt.Errorf("bad submit data %q", data)
	}
	return postID, parts[1], parts[2] == "1", nil
}

func (b *Bot) onSubmitButton(ctx context.Context, cq *models.CallbackQuery, messageID int) {
	postID, hash, approve, err := parseSubmitData(cq.Data)
	if err != nil {
		b.answer(ctx, cq, "Кнопка устарела", false)
		return
	}

	b.mu.Lock()
	refusal := b.refusalLocked()
	b.mu.Unlock()
	if refusal != "" {
		b.answer(ctx, cq, refusal, true)
		return
	}

	post, err := b.store.LatestWeekPost(ctx, b.userID)
	if err != nil || post.ID != postID || hashText(post.Text) != hash {
		b.answer(ctx, cq, "Пост изменился — вот свежее превью", false)
		b.sendPreview(ctx, true)
		return
	}
	b.answer(ctx, cq, "", false)
	if messageID != 0 {
		// Убираем кнопки с превью, чтобы не нажать дважды
		b.removeKeyboard(ctx, messageID)
	}

	prompt, err := b.api.SendMessage(ctx, &tgbot.SendMessageParams{
		ChatID: b.cfg.OwnerID,
		Text: "🔑 Пришли пароль-фразу ответом на это сообщение. Жду 2 минуты, сообщение с фразой сразу удалю.\n\n" +
			"Отправляй фразу, только если сам только что нажал кнопку.",
		ReplyMarkup: models.ForceReply{ForceReply: true, InputFieldPlaceholder: "пароль-фраза"},
	})
	if err != nil {
		slog.Error("send passphrase prompt", "error", b.redact(err.Error()))
		return
	}

	b.mu.Lock()
	b.pending = &pendingSubmit{
		postID: postID, hash: hash, approve: approve,
		promptID: prompt.ID, expires: time.Now().Add(passphraseTimeout),
	}
	b.mu.Unlock()
}

// refusalLocked — причина не начинать отправку; вызывать под b.mu
func (b *Bot) refusalLocked() string {
	switch {
	case b.running:
		return "Уже отправляю, дождись окончания"
	case b.locked:
		return "🔒 Отправка заблокирована после ошибки доступа. Проверь доступы и пришли /unlock"
	}
	cutoff := time.Now().Add(-time.Hour)
	recent := b.attempts[:0]
	for _, a := range b.attempts {
		if a.After(cutoff) {
			recent = append(recent, a)
		}
	}
	b.attempts = recent
	if len(b.attempts) >= maxAttemptsPerHour {
		return fmt.Sprintf("Не больше %d попыток в час: каждая — логин в домен и пуш MFA. Попробуй позже", maxAttemptsPerHour)
	}
	return ""
}

// takePassphrase забирает сообщение как пароль-фразу, если её ждут. Сообщение удаляется сразу,
// в логи и базу фраза не попадает
func (b *Bot) takePassphrase(ctx context.Context, msg *models.Message) bool {
	b.mu.Lock()
	p := b.pending
	if p == nil || strings.HasPrefix(msg.Text, "/") {
		b.mu.Unlock()
		return false
	}
	b.pending = nil
	expired := time.Now().After(p.expires)
	refusal := ""
	if !expired {
		refusal = b.refusalLocked()
		if refusal == "" {
			b.running = true
			b.attempts = append(b.attempts, time.Now())
		}
	}
	b.mu.Unlock()

	passphrase := []byte(msg.Text)
	deleteErr := b.deleteMessage(ctx, msg.Chat.ID, msg.ID)
	if err := b.deleteMessage(ctx, b.cfg.OwnerID, p.promptID); err != nil {
		slog.Error("delete passphrase prompt", "error", err)
	}
	if deleteErr != nil {
		slog.Error("delete passphrase message", "error", deleteErr)
		b.notify(ctx, "⚠️ Не смог удалить сообщение с пароль-фразой — удали его сам прямо сейчас!")
	}

	switch {
	case expired:
		clear(passphrase)
		b.notify(ctx, "⌛ Время на пароль-фразу вышло, нажми кнопку отправки ещё раз.")
	case refusal != "":
		clear(passphrase)
		b.notify(ctx, refusal)
	default:
		b.runSubmit(ctx, *p, passphrase)
	}
	return true
}

func (b *Bot) cancelPending(ctx context.Context) {
	b.mu.Lock()
	p := b.pending
	b.pending = nil
	b.mu.Unlock()
	if p == nil {
		b.notify(ctx, "Нечего отменять.")
		return
	}
	if err := b.deleteMessage(ctx, b.cfg.OwnerID, p.promptID); err != nil {
		slog.Error("delete passphrase prompt", "error", err)
	}
	b.notify(ctx, "Отменил, пароль-фразу больше не жду.")
}

// runSubmit отправляет неделю. Вызывается с b.running = true и сбрасывает его в конце
func (b *Bot) runSubmit(ctx context.Context, p pendingSubmit, passphrase []byte) {
	defer clear(passphrase)
	defer func() {
		b.mu.Lock()
		b.running = false
		b.mu.Unlock()
	}()

	status, err := b.send(ctx, "⏳ Начинаю отправку…", nil)
	if err != nil {
		slog.Error("send status", "error", err)
		return
	}
	progress := func(text string) { b.edit(ctx, status.ID, html.EscapeString(text), nil) }

	w, err := b.loadWeek(ctx)
	if err != nil || w.post.ID != p.postID || hashText(w.post.Text) != p.hash {
		progress("Пост изменился после превью — ничего не отправил. Пришли /submit ещё раз.")
		return
	}
	job := corp.Job{RequestApproval: p.approve, KnownPeriods: w.unapproved}
	for _, wl := range w.pendingWorklogs() {
		job.Worklogs = append(job.Worklogs, corp.WorklogJob{
			IssueKey: wl.IssueKey, Date: wl.Date, Duration: wl.Duration(), Comment: wl.Comment(),
		})
	}
	if len(job.Worklogs) == 0 && (!p.approve || len(job.KnownPeriods) == 0) {
		progress("Нечего отправлять: всё уже в Jira.")
		return
	}

	hooks := corp.Hooks{
		Progress: progress,
		Created: func(r corp.WorklogResult) error {
			_, err := b.store.SaveSubmission(ctx, store.Submission{
				UserID: b.userID, WeekPostID: w.post.ID, IssueKey: r.Job.IssueKey, WorkDate: r.Job.Date,
				Duration: r.Job.Duration, Comment: r.Job.Comment, WorklogID: r.WorklogID, PeriodID: r.PeriodID,
			})
			return err
		},
	}
	outcome, runErr := b.runner.Run(ctx, passphrase, job, hooks)
	for _, period := range outcome.Approved {
		if err := b.store.MarkPeriodRequested(ctx, b.userID, period); err != nil {
			slog.Error("mark period requested", "period", period, "error", err)
		}
	}

	if isAuthError(runErr) {
		b.mu.Lock()
		b.locked = true
		b.mu.Unlock()
	}
	if runErr != nil {
		slog.Error("submit", "error", runErr)
	}
	progress("Отправка завершена, итог ниже.")
	b.notify(ctx, renderOutcome(outcome, runErr))
}

func isAuthError(err error) bool {
	return errors.Is(err, jira.ErrAuth) || errors.Is(err, corp.ErrVPNAuth)
}

func renderOutcome(o corp.Outcome, runErr error) string {
	var b strings.Builder
	if runErr == nil {
		b.WriteString("✅ <b>Готово</b>\n")
	} else {
		b.WriteString("❌ <b>Остановился:</b> " + describeError(runErr) + "\n")
	}

	for _, r := range o.Worklogs {
		var state string
		switch r.Status {
		case corp.StatusCreated:
			state = "записано"
		case corp.StatusAlreadyInJira:
			state = "уже было в Jira (не ботом), пропустил"
		case corp.StatusFailed:
			state = "ошибка"
		default:
			state = "не дошёл"
		}
		fmt.Fprintf(&b, "• %s %s %s — %s\n", r.Job.Date.Format("02.01"), html.EscapeString(r.Job.IssueKey),
			parser.FormatDuration(r.Job.Duration), state)
	}
	for _, p := range o.Approved {
		fmt.Fprintf(&b, "📨 Период %d отправлен на проверку\n", p)
	}
	if o.CalendarURL != "" {
		fmt.Fprintf(&b, "\n<a href=\"%s\">Календарь WorklogPRO</a>", html.EscapeString(o.CalendarURL))
	}
	return b.String()
}

// describeError переводит известные ошибки на человеческий; наши ошибки не содержат секретов и тел ответов
func describeError(err error) string {
	switch {
	case errors.Is(err, corp.ErrBadPassphrase):
		return "пароль-фраза не подошла."
	case isAuthError(err):
		return "доступ не принят (" + html.EscapeString(err.Error()) + ").\n" +
			"🔒 Отправка заблокирована, чтобы не словить капчу или блокировку учётки. Проверь доступы и пришли /unlock"
	case errors.Is(err, jira.ErrUnknownResult):
		return "Jira ответила непонятно — worklog мог записаться, проверь календарь."
	case errors.Is(err, jira.ErrIssueNotFound):
		return html.EscapeString(err.Error()) + " — проверь ключ задачи в посте."
	case errors.Is(err, context.DeadlineExceeded):
		return "не дождался ответа. Если ждал VPN — возможно, пуш не подтверждён."
	default:
		return html.EscapeString(err.Error())
	}
}

func (b *Bot) cmdUnlock(ctx context.Context) {
	b.mu.Lock()
	was := b.locked
	b.locked = false
	b.mu.Unlock()
	if !was {
		b.notify(ctx, "Блокировки нет.")
		return
	}
	b.notify(ctx, "🔓 Блокировку снял. Если доступ снова не примут — заблокирую опять.")
}
