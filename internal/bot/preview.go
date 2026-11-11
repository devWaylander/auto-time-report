package bot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-telegram/bot/models"

	"github.com/devWaylander/auto-time-report/internal/calendar"
	"github.com/devWaylander/auto-time-report/internal/parser"
	"github.com/devWaylander/auto-time-report/internal/store"
)

const (
	submitPrefix = "s:"
	hideData     = "x"
	// Превью по кнопке: viewStatus и viewSubmit перерисовывают это же сообщение, viewNew присылает новое
	viewStatus = "v:status"
	viewSubmit = "v:submit"
	viewNew    = "v:new"
)

// weekState - пост недели вместе с тем, что из него уже записано в Jira
type weekState struct {
	post   store.WeekPost
	report parser.Report
	// Уже записанные worklog'и по ключу "задача|дата"
	submitted map[string]store.Submission
	// Периоды с записанными worklog'ами, ещё не отправленные на проверку
	unapproved []int64
}

func submissionKey(issue string, date time.Time) string {
	return issue + "|" + date.Format("2006-01-02")
}

// pendingWorklogs - worklog'и из поста, которых ещё нет в Jira
func (w weekState) pendingWorklogs() []parser.Worklog {
	var out []parser.Worklog
	for _, wl := range w.report.Worklogs {
		if _, ok := w.submitted[submissionKey(wl.IssueKey, wl.Date)]; !ok {
			out = append(out, wl)
		}
	}
	return out
}

var errNoPost = errors.New("no week post")

// loadWeek читает последний пост и сверяет его с уже записанным. Ошибки формата поста возвращаются как есть
func (b *Bot) loadWeek(ctx context.Context) (weekState, error) {
	post, err := b.store.LatestWeekPost(ctx, b.userID)
	if errors.Is(err, store.ErrNotFound) {
		return weekState{}, errNoPost
	}
	if err != nil {
		return weekState{}, err
	}
	report, err := parser.Parse(post.Text, b.cfg.DefaultIssue, b.now())
	if err != nil {
		return weekState{post: post}, err
	}

	dates := report.Dates()
	subs, err := b.store.SubmissionsBetween(ctx, b.userID, dates[0], dates[len(dates)-1])
	if err != nil {
		return weekState{}, err
	}
	w := weekState{post: post, report: report, submitted: map[string]store.Submission{}}
	for _, s := range subs {
		w.submitted[submissionKey(s.IssueKey, s.WorkDate)] = s
		// У найденных в Jira записей периода нет: на проверку их отправляют вместе с остальными днями
		if s.Manual || slices.Contains(w.unapproved, s.PeriodID) {
			continue
		}
		requested, err := b.store.PeriodRequested(ctx, b.userID, s.PeriodID)
		if err != nil {
			return weekState{}, err
		}
		if !requested {
			w.unapproved = append(w.unapproved, s.PeriodID)
		}
	}
	return w, nil
}

// sendPreview присылает превью недели; submit - с кнопками отправки
func (b *Bot) sendPreview(ctx context.Context, submit bool) {
	text, markup := b.buildPreview(ctx, submit)
	if _, err := b.send(ctx, text, markup); err != nil {
		slog.Error("send preview", "error", err)
	}
}

func (b *Bot) buildPreview(ctx context.Context, submit bool) (string, models.ReplyMarkup) {
	// Даже без превью оставляем "Обновить": поправил пост - нажал, не набирая команду заново
	refreshOnly := previewKeyboard(submit, 0, "", 0, 0, false)
	w, err := b.loadWeek(ctx)
	switch {
	case errors.Is(err, errNoPost):
		return "Поста текущей недели в канале пока нет", refreshOnly
	case err != nil && w.post.ID != 0:
		return postErrorsText("⚠️ В посте недели ошибки, предпросмотр не собран:", w.post, err), refreshOnly
	case err != nil:
		slog.Error("load week", "error", err)
		return "⚠️ Предпросмотр не собран, подробности в логах бота", refreshOnly
	}

	in := b.previewInput(ctx, w)
	in.Submit = submit
	return renderPreview(in), previewKeyboard(submit, w.post.ID, hashText(w.post.Text), in.Pending, in.Unapproved, !in.Unfilled)
}

// dayLine - строка превью за один день
type dayLine struct {
	Date      time.Time
	Kind      calendar.DayKind
	Total     time.Duration
	Issues    []string // "ABC-1 6ч", если задач за день больше одной
	Submitted int      // сколько worklog'ов дня уже в Jira
	Manual    int      // из них найдено в Jira, но поставлено не ботом
	Worklogs  int
}

type previewInput struct {
	Days         []dayLine
	Total        time.Duration
	Norm         time.Duration
	Missing      []time.Time
	Unfilled     bool // есть рабочие дни без записей, в том числе будущие: на проверку рано
	Pending      int
	Unapproved   int
	CalendarDown bool
	PostURL      string // ссылка на пост недели в канале; пусто - без ссылки
	Today        time.Time
	Submit       bool // превью с кнопками отправки
}

func (b *Bot) previewInput(ctx context.Context, w weekState) previewInput {
	dates := w.report.Dates()
	from, to := weekStart(dates[0]), weekStart(dates[len(dates)-1]).AddDate(0, 0, 6)
	today := b.today()

	in := previewInput{Today: today}
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		kind, err := b.cal.Day(ctx, d)
		if err != nil {
			in.CalendarDown = true
		}
		line := dayLine{Date: d, Kind: kind}
		for _, wl := range w.report.Worklogs {
			if !sameDay(wl.Date, d) {
				continue
			}
			line.Total += wl.Duration()
			line.Worklogs++
			line.Issues = append(line.Issues, wl.IssueKey+" "+parser.FormatDuration(wl.Duration()))
			if s, ok := w.submitted[submissionKey(wl.IssueKey, wl.Date)]; ok {
				line.Submitted++
				if s.Manual {
					line.Manual++
				}
			}
		}
		if len(line.Issues) < 2 {
			line.Issues = nil
		}

		in.Norm += kind.Norm()
		in.Total += line.Total
		if kind != calendar.Off && line.Total == 0 && !b.onVacation(ctx, d) {
			in.Unfilled = true
			if !d.After(today) {
				in.Missing = append(in.Missing, d)
			}
		}
		if kind != calendar.Off || line.Total > 0 {
			in.Days = append(in.Days, line)
		}
	}
	in.PostURL = postURL(w.post.ChatID, w.post.MessageID)
	in.Pending = len(w.pendingWorklogs())
	in.Unapproved = len(w.unapproved)
	return in
}

// onVacation - день в отпуске из /vacation. Ошибка базы не мешает превью: день считается рабочим
func (b *Bot) onVacation(ctx context.Context, d time.Time) bool {
	ok, err := b.store.OnVacation(ctx, b.userID, d)
	if err != nil {
		slog.Error("check vacation", "error", err)
	}
	return ok
}

var weekdays = [...]string{"Вс", "Пн", "Вт", "Ср", "Чт", "Пт", "Сб"}

func renderPreview(in previewInput) string {
	var b strings.Builder
	first, last := in.Days[0].Date, in.Days[len(in.Days)-1].Date
	fmt.Fprintf(&b, "<b>Неделя %s-%s</b>", first.Format("02.01"), last.Format("02.01.2006"))
	if in.PostURL != "" {
		fmt.Fprintf(&b, " · <a href=\"%s\">пост</a>", html.EscapeString(in.PostURL))
	}
	b.WriteString("\n\n")

	for _, d := range in.Days {
		mark := ""
		switch {
		case d.Worklogs > 0 && d.Submitted == d.Worklogs:
			mark = " ✅"
		case d.Submitted > 0:
			mark = " ◐"
		}
		note := ""
		switch {
		case d.Kind == calendar.Off && d.Total > 0:
			note = " <i>(выходной)</i>"
		case d.Kind == calendar.Short:
			note = " <i>(сокращённый)</i>"
		}
		// Пустой день - просто прочерк: "0ч" читался бы как сознательно записанный ноль
		total := "-"
		if d.Total > 0 {
			total = "- " + parser.FormatDuration(d.Total)
		}
		if d.Manual > 0 {
			mark += " вручную"
		}
		if d.Date.Equal(in.Today) {
			mark += " · сегодня"
		}
		fmt.Fprintf(&b, "%s %s %s%s%s\n", weekdays[d.Date.Weekday()], d.Date.Format("02.01"), total, note, mark)
		if len(d.Issues) > 0 {
			fmt.Fprintf(&b, "    <i>%s</i>\n", html.EscapeString(strings.Join(d.Issues, ", ")))
		}
	}

	fmt.Fprintf(&b, "\n<b>Итого: %s</b> из %s по норме", parser.FormatDuration(in.Total), parser.FormatDuration(in.Norm))
	switch {
	case in.Total < in.Norm:
		fmt.Fprintf(&b, " (не хватает %s)", parser.FormatDuration(in.Norm-in.Total))
	case in.Total > in.Norm:
		fmt.Fprintf(&b, " (переработка %s)", parser.FormatDuration(in.Total-in.Norm))
	}
	b.WriteString("\n")

	if len(in.Missing) > 0 {
		days := make([]string, len(in.Missing))
		for i, d := range in.Missing {
			days[i] = d.Format("02.01")
		}
		fmt.Fprintf(&b, "⚠️ Нет записей за рабочие дни: %s\n", strings.Join(days, ", "))
	}
	if in.CalendarDown {
		b.WriteString("⚠️ Производственный календарь недоступен, норма посчитана по пятидневке\n")
	}

	switch {
	case in.Pending > 0:
		fmt.Fprintf(&b, "\nК записи в Jira: %d", in.Pending)
	case in.Unfilled:
		b.WriteString("\n✅ В Jira есть всё из поста")
	case in.Unapproved > 0:
		b.WriteString("\nНеделя заполнена, можно отправить на проверку")
	default:
		b.WriteString("\n✅ Всё записано и отправлено на проверку")
	}
	if in.Submit && in.Unfilled && (in.Pending > 0 || in.Unapproved > 0) {
		b.WriteString("\nНа проверку после заполнения всех рабочих дней")
	}
	return b.String()
}

// previewKeyboard - кнопки под превью. В режиме отправки - кнопки записи, если есть что записывать,
// в режиме статуса - переход к отправке. На проверку - только когда заполнены все рабочие дни (filled).
// Внизу всегда "Обновить"
func previewKeyboard(submit bool, postID int64, hash string, pending, unapproved int, filled bool) models.ReplyMarkup {
	refresh := models.InlineKeyboardButton{Text: "🔄 Обновить", CallbackData: viewStatus}
	if submit {
		refresh.CallbackData = viewSubmit
	}
	nav := []models.InlineKeyboardButton{refresh}
	if !submit {
		if pending > 0 || unapproved > 0 && filled {
			nav = append(nav, models.InlineKeyboardButton{Text: "📨 К отправке", CallbackData: viewSubmit})
		}
		return models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{nav}}
	}
	nav = append(nav, models.InlineKeyboardButton{Text: "✖️ Скрыть", CallbackData: hideData})

	data := func(approve bool) string {
		a := "0"
		if approve {
			a = "1"
		}
		return fmt.Sprintf("%s%d:%s:%s", submitPrefix, postID, hash, a)
	}
	var row []models.InlineKeyboardButton
	switch {
	case pending > 0:
		row = append(row, models.InlineKeyboardButton{Text: "✍️ Записать", CallbackData: data(false)})
		if filled {
			row = append(row, models.InlineKeyboardButton{Text: "📨 Записать и на проверку", CallbackData: data(true)})
		}
	case unapproved > 0 && filled:
		row = append(row, models.InlineKeyboardButton{Text: "📨 Отправить на проверку", CallbackData: data(true)})
	default:
		return models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{nav}}
	}
	return models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{row, nav}}
}

// statusButton - "Статус" под напоминаниями и итогом отправки: присылает свежее превью отдельным сообщением
var statusButton = models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{
	{{Text: "📊 Статус", CallbackData: viewNew}},
}}

// postURL - ссылка на сообщение канала. Вид t.me/c/... работает и для закрытых каналов, если ты в них
// состоишь. У каналов ID начинается с -100, у остальных чатов таких ссылок нет
// postErrorsText - заголовок, ошибки разбора поста и ссылка на сам пост, если её можно собрать
func postErrorsText(header string, post store.WeekPost, err error) string {
	text := header + "\n" + formatParseErrors(err)
	if u := postURL(post.ChatID, post.MessageID); u != "" {
		text += fmt.Sprintf("\n\n<a href=\"%s\">Открыть пост</a>", html.EscapeString(u))
	}
	return text
}

func postURL(chatID, messageID int64) string {
	id, ok := strings.CutPrefix(strconv.FormatInt(chatID, 10), "-100")
	if !ok || id == "" || messageID <= 0 {
		return ""
	}
	return fmt.Sprintf("https://t.me/c/%s/%d", id, messageID)
}

// hashText - короткий отпечаток текста поста: кнопка превью сработает, только если пост с тех пор не менялся
func hashText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:5])
}

func weekStart(d time.Time) time.Time {
	offset := (int(d.Weekday()) + 6) % 7
	return d.AddDate(0, 0, -offset)
}

// DailyReminder - напоминание notify: за сегодня в посте нет записей
func (b *Bot) DailyReminder(ctx context.Context, date time.Time) error {
	_, err := b.send(ctx, fmt.Sprintf("📝 В посте недели отсутствуют записи за сегодня (%s %s)",
		strings.ToLower(weekdays[date.Weekday()]), date.Format("02.01")), statusButton)
	return err
}

// WeeklyPreview - напоминание notify: превью недели с кнопками
func (b *Bot) WeeklyPreview(ctx context.Context) error {
	text, markup := b.buildPreview(ctx, true)
	_, err := b.send(ctx, "🗓 Конец рабочей недели, подготовлен предпросмотр перед отправкой\n\n"+text, markup)
	return err
}
