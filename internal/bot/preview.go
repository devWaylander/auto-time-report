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
	// "Обновить" под уведомлением о проблемах в посте: n:<ID поста>
	noticePrefix = "n:"
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

// worklog - запись поста по задаче и дню
func (w weekState) worklog(issue string, date time.Time) (parser.Worklog, bool) {
	for _, wl := range w.report.Worklogs {
		if wl.IssueKey == issue && sameDay(wl.Date, date) {
			return wl, true
		}
	}
	return parser.Worklog{}, false
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

	// Целые недели, как в превью: записанный в Jira день мог пропасть из поста
	dates := report.Dates()
	subs, err := b.store.SubmissionsBetween(ctx, b.userID, weekStart(dates[0]), weekStart(dates[len(dates)-1]).AddDate(0, 0, 6))
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
	if err := b.showPreview(ctx, submit, 0, ""); err != nil {
		slog.Error("send preview", "error", err)
	}
}

// showPreview показывает превью в сообщении messageID или новым сообщением (messageID = 0), header - строка
// над ним. Если в посте ошибки, вместо превью показывается уведомление о посте: ошибки всегда в одном
// сообщении, которое бот обновит или удалит после исправления
func (b *Bot) showPreview(ctx context.Context, submit bool, messageID int, header string) error {
	w, err := b.loadWeek(ctx)
	if err != nil && w.post.ID != 0 {
		return b.showErrorNotice(ctx, w.post, header, messageID)
	}
	text, markup := b.renderWeek(ctx, w, err, submit)
	if header != "" {
		text = header + "\n\n" + text
	}
	if messageID != 0 {
		b.edit(ctx, messageID, text, markup)
		return nil
	}
	_, err = b.send(ctx, text, markup)
	return err
}

func (b *Bot) buildPreview(ctx context.Context, submit bool) (string, models.ReplyMarkup) {
	w, err := b.loadWeek(ctx)
	return b.renderWeek(ctx, w, err, submit)
}

// renderWeek - текст и кнопки превью по результату loadWeek
func (b *Bot) renderWeek(ctx context.Context, w weekState, err error, submit bool) (string, models.ReplyMarkup) {
	// Даже без превью оставляем "Обновить": поправил пост - нажал, не набирая команду заново
	keys := previewKeys{Submit: submit, PostURL: postURL(w.post.ChatID, w.post.MessageID)}
	switch {
	case errors.Is(err, errNoPost):
		return "Поста текущей недели в канале пока нет", previewKeyboard(keys)
	case err != nil && w.post.ID != 0:
		return "⚠️ В посте недели ошибки, предпросмотр не собран:\n" + formatParseErrors(err), previewKeyboard(keys)
	case err != nil:
		slog.Error("load week", "error", err)
		return "⚠️ Предпросмотр не собран, подробности в логах бота", previewKeyboard(keys)
	}

	in := b.previewInput(ctx, w)
	in.Submit = submit
	keys.PostID, keys.Hash = w.post.ID, hashText(w.post.Text)
	keys.Pending, keys.Unapproved, keys.Filled = in.Pending, in.Unapproved, !in.Unfilled
	return renderPreview(in), previewKeyboard(keys)
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
	Vacation  bool          // рабочий день в отпуске из /vacation
	InJira    time.Duration // сколько за день записано в Jira
	Mismatch  bool          // в Jira не то, что в посте: время другое или запись из поста пропала
}

// shortfall - сколько не хватает до нормы дня; ноль - день пустой, в отпуске или норма набрана
func (d dayLine) shortfall() time.Duration {
	if d.Total == 0 || d.Vacation || d.Total >= d.Kind.Norm() {
		return 0
	}
	return d.Kind.Norm() - d.Total
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
		line := dayLine{Date: d, Kind: kind, Vacation: kind != calendar.Off && b.onVacation(ctx, d)}
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
		for _, s := range w.submitted {
			if !sameDay(s.WorkDate, d) {
				continue
			}
			line.InJira += s.Duration
			if wl, ok := w.worklog(s.IssueKey, d); !ok || wl.Duration() != s.Duration {
				line.Mismatch = true
			}
		}

		in.Norm += kind.Norm()
		in.Total += line.Total
		if kind != calendar.Off && line.Total == 0 && !line.Vacation {
			in.Unfilled = true
			if !d.After(today) {
				in.Missing = append(in.Missing, d)
			}
		}
		if kind != calendar.Off || line.Total > 0 || line.InJira > 0 {
			in.Days = append(in.Days, line)
		}
	}
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
	fmt.Fprintf(&b, "<b>Неделя %s-%s</b>\n\n", first.Format("02.01"), last.Format("02.01.2006"))

	for _, d := range in.Days {
		// Значок в начале каждой строки: эмодзи выше текста, и строки без него выходили бы ниже остальных
		mark := "▫️"
		switch {
		case d.Mismatch:
			mark = "⚠️"
		case d.Worklogs > 0 && d.Submitted == d.Worklogs:
			mark = "☑️"
		case d.Submitted > 0:
			mark = "🔸"
		case d.Worklogs > 0 && d.Date.Equal(in.Today):
			// Сегодняшний день ещё дописывается
			mark = "✏️"
		case d.Worklogs > 0:
			mark = "🔹"
		}
		tail := ""
		note := ""
		switch {
		case d.Kind == calendar.Off && d.Total > 0:
			note = " <i>(выходной)</i>"
		case d.Kind == calendar.Short:
			note = " <i>(сокращённый)</i>"
		}
		// У пустого дня времени нет совсем: "0ч" читался бы как сознательно записанный ноль, а пустоту
		// и так показывает значок
		total := ""
		if d.Total > 0 {
			total = " - " + parser.FormatDuration(d.Total)
		}
		if d.shortfall() > 0 {
			total += " из " + parser.FormatDuration(d.Kind.Norm())
		}
		if d.Manual > 0 {
			tail += " вручную"
		}
		if d.Mismatch {
			tail += " · в Jira " + parser.FormatDuration(d.InJira)
		}
		if d.Date.Equal(in.Today) {
			tail += " · сегодня"
		}
		fmt.Fprintf(&b, "%s %s %s%s%s%s\n", mark, weekdays[d.Date.Weekday()], d.Date.Format("02.01"), total, note, tail)
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
	// Внутри одной недели хватает дня недели; если превью шире, к нему добавляется дата
	oneWeek := weekStart(first).Equal(weekStart(last))
	var short []string
	for _, d := range in.Days {
		if s := d.shortfall(); s > 0 {
			label := weekdays[d.Date.Weekday()]
			if !oneWeek {
				label += " " + d.Date.Format("02.01")
			}
			short = append(short, label+"-"+parser.FormatDuration(s))
		}
	}
	if len(short) > 0 {
		fmt.Fprintf(&b, "⚠️ Недобор: %s\n", strings.Join(short, ", "))
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

// previewKeys - из чего собираются кнопки под превью
type previewKeys struct {
	Submit     bool // режим отправки
	PostID     int64
	Hash       string
	PostURL    string // кнопка "Пост"; пусто - без неё
	Pending    int
	Unapproved int
	Filled     bool // все рабочие дни заполнены: можно на проверку
}

// previewKeyboard - кнопки под превью. Сверху действие: в режиме отправки - запись, если есть что
// записывать, в режиме статуса - переход к отправке. На проверку - только когда заполнены все рабочие дни.
// Снизу всегда "Обновить" и "Пост"
func previewKeyboard(k previewKeys) models.ReplyMarkup {
	refresh := models.InlineKeyboardButton{Text: "🔄 Обновить", CallbackData: viewStatus}
	if k.Submit {
		refresh.CallbackData = viewSubmit
	}
	nav := append([]models.InlineKeyboardButton{refresh}, postButton(k.PostURL)...)

	var row []models.InlineKeyboardButton
	if !k.Submit {
		if k.Pending > 0 || k.Unapproved > 0 && k.Filled {
			row = append(row, models.InlineKeyboardButton{Text: "📨 К отправке", CallbackData: viewSubmit})
		}
	} else {
		nav = append(nav, models.InlineKeyboardButton{Text: "✖️ Скрыть", CallbackData: hideData})
		data := func(approve bool) string {
			a := "0"
			if approve {
				a = "1"
			}
			return fmt.Sprintf("%s%d:%s:%s", submitPrefix, k.PostID, k.Hash, a)
		}
		switch {
		case k.Pending > 0:
			row = append(row, models.InlineKeyboardButton{Text: "✍️ Записать", CallbackData: data(false)})
			if k.Filled {
				row = append(row, models.InlineKeyboardButton{Text: "📨 Записать и на проверку", CallbackData: data(true)})
			}
		case k.Unapproved > 0 && k.Filled:
			row = append(row, models.InlineKeyboardButton{Text: "📨 Отправить на проверку", CallbackData: data(true)})
		}
	}
	if len(row) == 0 {
		return models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{nav}}
	}
	return models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{row, nav}}
}

// postButton - кнопка-ссылка на пост недели; без ссылки кнопки нет
func postButton(url string) []models.InlineKeyboardButton {
	if url == "" {
		return nil
	}
	return []models.InlineKeyboardButton{{Text: "🔗 Пост", URL: url}}
}

// statusKeyboard - "Статус" под напоминаниями и итогом отправки: присылает свежее превью отдельным
// сообщением. Рядом "Пост", если передана ссылка
func statusKeyboard(postURL string) models.ReplyMarkup {
	row := append([]models.InlineKeyboardButton{{Text: "📊 Статус", CallbackData: viewNew}}, postButton(postURL)...)
	return models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{row}}
}

// postURL - ссылка на сообщение канала. Вид t.me/c/... работает и для закрытых каналов, если читатель
// в них состоит. У каналов ID начинается с -100, у остальных чатов таких ссылок нет
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

// DailyReminder - напоминание notify: за сегодня в посте нет записей. Кнопка "Пост" ведёт к последнему
// посту: если за эту неделю его ещё нет, это место в канале рядом с концом ленты
func (b *Bot) DailyReminder(ctx context.Context, date time.Time) error {
	url := ""
	if post, err := b.store.LatestWeekPost(ctx, b.userID); err == nil {
		url = postURL(post.ChatID, post.MessageID)
	} else if !errors.Is(err, store.ErrNotFound) {
		slog.Error("load week post", "error", err)
	}
	_, err := b.send(ctx, fmt.Sprintf("📝 В посте недели отсутствуют записи за сегодня (%s %s)",
		strings.ToLower(weekdays[date.Weekday()]), date.Format("02.01")), statusKeyboard(url))
	return err
}

// WeeklyPreview - напоминание notify: превью недели с кнопками
func (b *Bot) WeeklyPreview(ctx context.Context) error {
	return b.showPreview(ctx, true, 0, "🗓 Конец рабочей недели, подготовлен предпросмотр перед отправкой")
}
