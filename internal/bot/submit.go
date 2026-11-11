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

// pendingSubmit - нажата кнопка отправки, ждём пароль-фразу
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
		b.answer(ctx, cq, "Пост изменился, свежий предпросмотр ниже", false)
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
		Text: "🔑 Ожидается пароль-фраза ответом на это сообщение в течение двух минут\n\n" +
			"Если кнопку никто не нажимал - фразу не отправлять",
		ReplyMarkup: models.ForceReply{ForceReply: true, InputFieldPlaceholder: "пароль-фраза"},
	})
	if err != nil {
		slog.Error("send passphrase prompt", "error", b.redact(err.Error()))
		return
	}

	b.mu.Lock()
	b.pending = &pendingSubmit{
		postID: postID, hash: hash, approve: approve,
		promptID: prompt.ID, expires: b.clock().Add(passphraseTimeout),
	}
	b.mu.Unlock()
}

// refusalLocked - причина не начинать отправку; вызывать под b.mu
func (b *Bot) refusalLocked() string {
	switch {
	case b.running:
		return "В текущий момент уже осуществляется отправка"
	case b.locked:
		return "🔒 Отправка заблокирована после ошибки доступа. Снять блокировку после проверки доступов /unlock"
	}
	cutoff := b.clock().Add(-time.Hour)
	recent := b.attempts[:0]
	for _, a := range b.attempts {
		if a.After(cutoff) {
			recent = append(recent, a)
		}
	}
	b.attempts = recent
	if len(b.attempts) >= maxAttemptsPerHour {
		return fmt.Sprintf("Лимит исчерпан: не больше %d попыток в час", maxAttemptsPerHour)
	}
	return ""
}

// takePassphrase забирает сообщение как пароль-фразу, если её ждут. Сообщение удаляется сразу,
// в логи и базу фраза не попадает
func (b *Bot) takePassphrase(ctx context.Context, msg *models.Message) bool {
	b.mu.Lock()
	p := b.pending
	// Пока ждём фразу, берём любой текст, кроме /cancel: фраза вполне может начинаться со слэша
	if p == nil || isCancel(msg.Text) {
		b.mu.Unlock()
		return false
	}
	b.pending = nil
	expired := b.clock().After(p.expires)
	refusal := ""
	if !expired {
		refusal = b.refusalLocked()
		if refusal == "" {
			b.running = true
			b.attempts = append(b.attempts, b.clock())
			b.inflight.Add(1)
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
		b.notify(ctx, "⚠️ Сообщение с пароль-фразой не удалилось - необходимо удалить вручную прямо сейчас!")
	}

	switch {
	case expired:
		clear(passphrase)
		b.notify(ctx, "⌛ Время на пароль-фразу истекло, нужна повторная отправка")
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
		b.notify(ctx, "Нечего отменять")
		return
	}
	if err := b.deleteMessage(ctx, b.cfg.OwnerID, p.promptID); err != nil {
		slog.Error("delete passphrase prompt", "error", err)
	}
	b.notify(ctx, "Отменено, пароль-фраза больше не ожидается")
}

func isCancel(text string) bool {
	cmd, _, _ := strings.Cut(strings.TrimSpace(text), " ")
	cmd, _, _ = strings.Cut(cmd, "@")
	return cmd == "/cancel"
}

// runSubmit отправляет неделю. Вызывается с b.running = true и b.inflight.Add(1), сбрасывает их в конце
func (b *Bot) runSubmit(ctx context.Context, p pendingSubmit, passphrase []byte) {
	defer clear(passphrase)
	defer func() {
		b.mu.Lock()
		b.running = false
		b.mu.Unlock()
		b.inflight.Done()
	}()
	// Запись в базу не отменяем даже при остановке бота: worklog уже в Jira, бот обязан о нём помнить
	dbCtx := context.WithoutCancel(ctx)

	status, err := b.send(ctx, "⏳ Отправка…", nil)
	if err != nil {
		slog.Error("send status", "error", err)
		return
	}
	// Шаги отправки дублируются в лог: если Telegram недоступен, по логу видно, докуда дошло
	progress := func(text string) {
		slog.Info("submit", "step", text)
		b.edit(ctx, status.ID, html.EscapeString(text), nil)
	}

	w, err := b.loadWeek(ctx)
	if err != nil || w.post.ID != p.postID || hashText(w.post.Text) != p.hash {
		progress("Пост изменился после предпросмотра, ничего не отправлено. Свежий предпросмотр - /submit")
		return
	}
	job := corp.Job{RequestApproval: p.approve, KnownPeriods: w.unapproved}
	for _, wl := range w.pendingWorklogs() {
		job.Worklogs = append(job.Worklogs, corp.WorklogJob{
			IssueKey: wl.IssueKey, Date: wl.Date, Duration: wl.Duration(), Comment: wl.Comment(),
		})
	}
	if len(job.Worklogs) == 0 && (!p.approve || len(job.KnownPeriods) == 0) {
		progress("Нечего отправлять: всё уже в Jira")
		return
	}

	hooks := corp.Hooks{
		Progress: progress,
		Created: func(r corp.WorklogResult) error {
			_, err := b.store.SaveSubmission(dbCtx, store.Submission{
				UserID: b.userID, WeekPostID: w.post.ID, IssueKey: r.Job.IssueKey, WorkDate: r.Job.Date,
				Duration: r.Job.Duration, Comment: r.Job.Comment, WorklogID: r.WorklogID, PeriodID: r.PeriodID,
			})
			return err
		},
		Found: func(r corp.WorklogResult) {
			// Запоминаем, чтобы превью считало день записанным и не звало в VPN ради него снова.
			// Не вышло - не страшно: при следующей отправке бот найдёт его в Jira заново
			_, err := b.store.SaveSubmission(dbCtx, store.Submission{
				UserID: b.userID, WeekPostID: w.post.ID, IssueKey: r.Job.IssueKey, WorkDate: r.Job.Date,
				Duration: r.InJira, Manual: true,
			})
			if err != nil {
				slog.Error("save worklog found in jira", "error", err)
			}
		},
	}
	outcome, runErr := b.runner.Run(ctx, passphrase, job, hooks)
	for _, period := range outcome.Approved {
		if err := b.store.MarkPeriodRequested(dbCtx, b.userID, period); err != nil {
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
	created := 0
	for _, r := range outcome.Worklogs {
		if r.Status == corp.StatusCreated {
			created++
		}
	}
	slog.Info("submit finished", "created", created, "approved", len(outcome.Approved))
	progress("Отправка завершена, итог ниже")
	if _, err := b.send(dbCtx, renderOutcome(outcome, runErr), statusButton); err != nil {
		slog.Error("send outcome", "error", err)
	}
}

func isAuthError(err error) bool {
	return errors.Is(err, jira.ErrAuth) || errors.Is(err, corp.ErrVPNAuth)
}

func renderOutcome(o corp.Outcome, runErr error) string {
	var b strings.Builder
	if runErr == nil {
		b.WriteString("✅ <b>Готово</b>\n")
	} else {
		fmt.Fprintf(&b, "❌ <b>Остановлено:</b> %s\n", describeError(runErr))
	}

	for _, r := range o.Worklogs {
		var state string
		switch r.Status {
		case corp.StatusCreated:
			state = "записано"
		case corp.StatusAlreadyInJira:
			state = "уже было в Jira вручную, пропущено"
			if r.InJira != r.Job.Duration {
				state += fmt.Sprintf(". ⚠️ В Jira %s, в посте %s", parser.FormatDuration(r.InJira), parser.FormatDuration(r.Job.Duration))
			}
		case corp.StatusFailed:
			state = "ошибка"
		default:
			state = "очередь не дошла"
		}
		fmt.Fprintf(&b, "• %s %s %s - %s\n", r.Job.Date.Format("02.01"), html.EscapeString(r.Job.IssueKey),
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
		return "пароль-фраза не подошла"
	case isAuthError(err):
		return "доступ не принят (" + html.EscapeString(err.Error()) + ")\n" +
			"🔒 Отправка заблокирована, чтобы не словить капчу или блокировку учётки. Снять блокировку после проверки доступов /unlock"
	case errors.Is(err, jira.ErrUnknownResult):
		return "нестандартный ответ от Jira, worklog мог записаться - нужна сверка с календарём"
	case errors.Is(err, jira.ErrIssueNotFound):
		return html.EscapeString(err.Error()) + " - возможно, ошибка в ключе задачи в посте"
	case errors.Is(err, context.DeadlineExceeded):
		return "истекло время ожидания. Вероятная причина - не подтверждён пуш Multifactor"
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
		b.notify(ctx, "Блокировки нет")
		return
	}
	b.notify(ctx, "🔓 Блокировка снята. При новой ошибке доступа отправка снова заблокируется")
}
