package bot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/devWaylander/auto-time-report/internal/calendar"
	"github.com/devWaylander/auto-time-report/internal/parser"
	"github.com/devWaylander/auto-time-report/internal/store"
)

const (
	submitPrefix = "s:"
	// "Обновить" под уведомлением о посте из прошлых версий бота: n:<ID поста>
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
	// Недели с подтверждённой отправкой периода на проверку
	requestedWeeks map[string]bool
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

const noWeekPostText = "Поста текущей недели в канале пока нет"
const emptyWeekPostText = "Пост недели найден, записей пока нет"

func (b *Bot) currentWeekPost(ctx context.Context) (store.WeekPost, error) {
	return b.store.WeekPostForWeek(ctx, b.userID, calendar.WeekStart(b.now()))
}

// loadWeek читает пост текущей недели и сверяет его с уже записанным
func (b *Bot) loadWeek(ctx context.Context) (weekState, error) {
	post, err := b.currentWeekPost(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return weekState{}, errNoPost
	}
	if err != nil {
		return weekState{}, err
	}
	return b.loadWeekOf(ctx, post)
}

// loadWeekOf - то же для конкретного поста
func (b *Bot) loadWeekOf(ctx context.Context, post store.WeekPost) (weekState, error) {
	report, err := parser.Parse(post.Text, b.cfg.DefaultIssue, b.now())
	if err != nil {
		return weekState{post: post}, err
	}

	// Целые недели, как в превью: записанный в Jira день мог пропасть из поста
	dates := report.Dates()
	subs, err := b.store.SubmissionsBetween(ctx, b.userID, calendar.WeekStart(dates[0]), calendar.WeekStart(dates[len(dates)-1]).AddDate(0, 0, 6))
	if err != nil {
		return weekState{}, err
	}
	w := weekState{post: post, report: report, submitted: map[string]store.Submission{}, requestedWeeks: map[string]bool{}}
	checkedPeriods := map[int64]bool{}
	for _, s := range subs {
		w.submitted[submissionKey(s.IssueKey, s.WorkDate)] = s
		// У найденных в Jira записей периода нет: на проверку их отправляют вместе с остальными днями
		if s.Manual {
			continue
		}
		requested, checked := checkedPeriods[s.PeriodID]
		if !checked {
			requested, err = b.store.PeriodRequested(ctx, b.userID, s.PeriodID)
			if err != nil {
				return weekState{}, err
			}
			checkedPeriods[s.PeriodID] = requested
			if !requested {
				w.unapproved = append(w.unapproved, s.PeriodID)
			}
		}
		if requested {
			w.requestedWeeks[calendar.WeekStart(s.WorkDate).Format(time.DateOnly)] = true
		}
	}
	return w, nil
}

// sendPreview присылает превью недели и сообщает, удалось ли его показать
func (b *Bot) sendPreview(ctx context.Context, submit bool) bool {
	if err := b.showPreview(ctx, submit, ""); err != nil {
		slog.Error("send preview", "error", err)
		return false
	}
	return true
}

func (b *Bot) buildPreview(ctx context.Context, submit bool) (string, models.ReplyMarkup) {
	w, err := b.loadWeek(ctx)
	v := b.viewOf(ctx, w, err, submit, "")
	return v.text, v.markup
}

// weekView - сообщение о посте: превью с предупреждениями под ним или ошибки разбора
type weekView struct {
	text         string
	markup       models.ReplyMarkup
	problems     string // ошибки разбора или предупреждения; пусто - с постом всё в порядке
	live         bool   // сообщение про конкретный пост: его можно держать живым
	archiveText  string // итог недели без отметки "сегодня"
	archiveKnown bool   // расчёт достоверен, пустой итог означает незавершённую неделю
}

// viewOf собирает сообщение по результату loadWeek; header - строка над ним
func (b *Bot) viewOf(ctx context.Context, w weekState, err error, submit bool, header string) weekView {
	// Даже без превью оставляем "Обновить": поправил пост - нажал, не набирая команду заново
	keys := previewKeys{Submit: submit, PostURL: postURL(w.post.ChatID, w.post.MessageID)}
	withHeader := func(text string) string {
		if header == "" {
			return text
		}
		return header + "\n\n" + text
	}
	switch {
	case errors.Is(err, errNoPost):
		keys.ChannelURL = b.weekChannelURL(ctx)
		return weekView{text: noWeekPostText, markup: previewKeyboard(keys)}
	case errors.Is(err, parser.ErrNoEntries):
		return weekView{text: withHeader(emptyWeekPostText), markup: previewKeyboard(keys), live: true, archiveKnown: true}
	case err != nil && w.post.ID != 0:
		problems := "⚠️ В посте недели ошибки:\n" + formatParseErrors(err)
		return weekView{text: withHeader(problems), markup: previewKeyboard(keys), problems: problems, live: true, archiveKnown: true}
	case err != nil:
		slog.Error("load week", "error", err)
		return weekView{text: "⚠️ Предпросмотр не собран, подробности в логах бота", markup: previewKeyboard(keys)}
	}

	in := b.previewInput(ctx, w)
	in.Submit = submit
	keys.PostID, keys.Hash = w.post.ID, hashText(w.post.Text)
	keys.Pending, keys.Unapproved, keys.Filled = in.Pending, in.Unapproved, !in.Unfilled
	text := renderPreview(in)
	problems := b.warnings(ctx, w.report)
	if problems != "" {
		text += "\n\n" + problems
	}
	view := weekView{
		text: withHeader(text), markup: previewKeyboard(keys), problems: problems, live: true,
		archiveKnown: !in.CalendarDown || in.Mismatch || in.Pending > 0,
	}
	if in.complete() {
		in.Today = time.Time{}
		view.archiveText = renderPreview(in)
	}
	return view
}

// warnings - предупреждения по разобранному посту: даты без записей и правки дней, уже записанных в Jira
func (b *Bot) warnings(ctx context.Context, report parser.Report) string {
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
	Requested    bool // есть подтверждение отправки для каждой недели в посте
	Mismatch     bool
	CalendarDown bool
	Today        time.Time
	Submit       bool // превью с кнопками отправки
}

func (in previewInput) complete() bool {
	return in.Pending == 0 && !in.Unfilled && !in.Mismatch && !in.CalendarDown && in.Requested
}

func (b *Bot) previewInput(ctx context.Context, w weekState) previewInput {
	dates := w.report.Dates()
	from, to := calendar.WeekStart(dates[0]), calendar.WeekStart(dates[len(dates)-1]).AddDate(0, 0, 6)
	today := b.today()

	in := previewInput{
		Today: today, Unapproved: len(w.unapproved),
		Requested: len(w.unapproved) == 0 && w.requestedWeeks[b.postWeek(w.post)],
	}
	for _, date := range dates {
		if !w.requestedWeeks[calendar.WeekStart(date).Format(time.DateOnly)] {
			in.Requested = false
			break
		}
	}
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
		in.Mismatch = in.Mismatch || line.Mismatch

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

const (
	weekDaysOpen    = "<blockquote><table compact>"
	weekDaysClose   = "</table></blockquote>"
	weekStatusEmoji = `[▫☑⚠✏✅🔸🔹]\x{FE0F}?`
	wordJoinerHTML  = "&#8288;"
)

var emojiOnlyWeekCell = regexp.MustCompile(`<td>(` + weekStatusEmoji + `)</td>`)
var savedWeekRow = regexp.MustCompile(`<tr><td>([^<&]+)</td><td>([^<&]*)</td><td>([^<]+)</td></tr>|<tr><td colspan="3">(↳ [^<]*)</td></tr>`)

func renderDayRow(date, total, status string) string {
	return "<tr><td>" + date + "</td><td>" + total + "</td><td>" + status + "</td></tr>"
}

func renderAlignedDayRow(date, total, status string, durationWidth int) string {
	const nbsp = "&#160;"
	prefix := strings.ReplaceAll(date, " ", nbsp)
	if total != "" {
		prefix += nbsp + "· " + strings.ReplaceAll(total, " ", nbsp)
	} else if durationWidth > 0 {
		prefix += nbsp + nbsp + " "
	}
	// Обычный пробел перед временем позволяет перенести его на узком экране
	prefix += strings.Repeat(nbsp, durationWidth-utf8.RuneCountInString(total)+2)
	return "<tr><td><code>" + prefix + "</code>" + status + "</td></tr>"
}

func renderDayIssues(issues string) string {
	return `<tr><td colspan="3">↳ ` + issues + "</td></tr>"
}

// alignWeekRows выравнивает значки по самой длинной длительности, в том числе в сохранённых итогах
func alignWeekRows(rows string) string {
	matched := savedWeekRow.FindAllStringSubmatch(rows, -1)
	var durationWidth, matchedBytes int
	for _, row := range matched {
		matchedBytes += len(row[0])
		durationWidth = max(durationWidth, utf8.RuneCountInString(row[2]))
	}
	if matchedBytes != len(rows) {
		// Не перестраиваем таблицу частично, если архив содержит другую разметку
		// U+2060 удерживает обычную высоту старых ячеек со значком на Android
		return emojiOnlyWeekCell.ReplaceAllString(rows, `<td>${1}`+wordJoinerHTML+`</td>`)
	}
	var aligned strings.Builder
	for _, row := range matched {
		if row[4] != "" {
			aligned.WriteString("<tr><td>" + row[4] + "</td></tr>")
		} else {
			aligned.WriteString(renderAlignedDayRow(row[1], row[2], row[3], durationWidth))
		}
	}
	return aligned.String()
}

// weekTableMessage переводит заголовок и итоги в абзацы расширенного сообщения
func weekTableMessage(text string) *models.InputRichMessage {
	header, rest, ok := strings.Cut(text, weekDaysOpen)
	if !ok {
		return nil
	}
	rows, footer, ok := strings.Cut(rest, weekDaysClose)
	if !ok {
		return nil
	}
	rows = alignWeekRows(rows)
	if summary := strings.TrimSpace(footer); summary != "" {
		// Невидимый символ сохраняет одну пустую строку перед итогами
		footer = wordJoinerHTML + "\n" + summary
	}
	paragraphs := func(text string) string {
		text = strings.ReplaceAll(strings.TrimSpace(text), "\n", "<br>")
		return "<p>" + strings.ReplaceAll(text, "<br><br>", "</p><p>") + "</p>"
	}
	return &models.InputRichMessage{
		HTML: paragraphs(header) + weekDaysOpen + rows + weekDaysClose + paragraphs(footer),
	}
}

func renderPreview(in previewInput) string {
	var b strings.Builder
	first, last := in.Days[0].Date, in.Days[len(in.Days)-1].Date
	fmt.Fprintf(&b, "<b>Неделя %s-%s</b>\n", first.Format("02.01"), last.Format("02.01.2006"))

	b.WriteString(weekDaysOpen)
	for _, d := range in.Days {
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
		var notes []string
		if d.shortfall() > 0 {
			notes = append(notes, "норма "+parser.FormatDuration(d.Kind.Norm()))
		}
		switch {
		case d.Kind == calendar.Off && d.Total > 0:
			notes = append(notes, "выходной")
		case d.Kind == calendar.Short:
			notes = append(notes, "сокращённый")
		}
		if d.Manual > 0 {
			notes = append(notes, "вручную")
		}
		if d.Mismatch {
			notes = append(notes, "в Jira "+parser.FormatDuration(d.InJira))
		}
		if d.Date.Equal(in.Today) {
			notes = append(notes, "сегодня")
		}
		date := weekdays[d.Date.Weekday()] + " " + d.Date.Format("02.01")
		var total string
		if d.Total > 0 {
			total = parser.FormatDuration(d.Total)
		}
		if len(notes) > 0 {
			mark += " " + strings.Join(notes, " · ")
		}
		b.WriteString(renderDayRow(date, total, mark))
		if len(d.Issues) > 0 {
			b.WriteString(renderDayIssues(html.EscapeString(strings.Join(d.Issues, ", "))))
		}
	}
	b.WriteString(weekDaysClose + "\n")

	fmt.Fprintf(&b, "<b>Итого: %s</b> из %s по норме", parser.FormatDuration(in.Total), parser.FormatDuration(in.Norm))
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
	oneWeek := calendar.WeekStart(first).Equal(calendar.WeekStart(last))
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
	case in.Mismatch:
		b.WriteString("\n⚠️ Есть расхождения с Jira")
	case in.complete():
		b.WriteString("\n✅ Всё записано и отправлено на проверку")
	case in.Unfilled:
		b.WriteString("\n✅ В Jira есть всё из поста")
	case in.Unapproved > 0:
		b.WriteString("\nНеделя заполнена, можно отправить на проверку")
	default:
		b.WriteString("\n✅ В Jira есть всё из поста")
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
	ChannelURL string // кнопка "В канал", когда ещё нет поста текущей недели
	Pending    int
	Unapproved int
	Filled     bool // все рабочие дни заполнены: можно на проверку
}

// previewKeyboard - кнопки под превью. Сверху действие: в режиме отправки - запись, если есть что
// записывать, в режиме статуса - переход к отправке. На проверку - только когда заполнены все рабочие дни.
// Снизу "Обновить" и ссылка на пост или канал
func previewKeyboard(k previewKeys) models.ReplyMarkup {
	refresh := models.InlineKeyboardButton{Text: "🔄 Обновить", CallbackData: viewStatus}
	if k.Submit {
		refresh.CallbackData = viewSubmit
	}
	nav := append([]models.InlineKeyboardButton{refresh}, postButton(k.PostURL)...)
	if k.ChannelURL != "" {
		nav = append(nav, models.InlineKeyboardButton{Text: "🔗 В канал", URL: k.ChannelURL})
	}

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

// weekChannelURL открывает последний пост или сам канал, если бот ещё не видел постов
func (b *Bot) weekChannelURL(ctx context.Context) string {
	post, err := b.store.LatestWeekPostInChannel(ctx, b.userID, b.cfg.ChannelID)
	if err == nil {
		if link := postURL(post.ChatID, post.MessageID); link != "" {
			return link
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		slog.Warn("load last post for channel link", "error", err)
	}

	chat, err := b.api.GetChat(ctx, &tgbot.GetChatParams{ChatID: b.cfg.ChannelID})
	if err != nil {
		slog.Warn("load channel link", "error", b.redact(err.Error()))
		return ""
	}
	if chat == nil {
		return ""
	}
	if chat.Username != "" {
		return "https://t.me/" + chat.Username
	}
	if chat.PinnedMessage != nil {
		if link := postURL(chat.ID, int64(chat.PinnedMessage.ID)); link != "" {
			return link
		}
	}
	return chat.InviteLink
}

// hashText - короткий отпечаток текста поста: кнопка превью сработает, только если пост с тех пор не менялся
func hashText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:5])
}

// DailyReminder сообщает об отсутствии поста текущей недели или записей за сегодня
func (b *Bot) DailyReminder(ctx context.Context, date time.Time) error {
	b.postMu.Lock()
	defer b.postMu.Unlock()
	if !sameDay(date, b.today()) {
		return nil
	}
	week := calendar.WeekStart(date)
	post, err := b.store.WeekPostForWeek(ctx, b.userID, week)
	if errors.Is(err, store.ErrNotFound) {
		if b.active.WeekStart.Format(time.DateOnly) == week.Format(time.DateOnly) && b.active.Header != "" {
			return nil
		}
		view := b.viewOf(ctx, weekState{}, errNoPost, false, "")
		view.text = "📝 " + view.text
		return b.showActive(ctx, view, store.ActiveWeekView{WeekStart: week})
	}
	if err != nil {
		return err
	}
	// Пост мог прийти после проверки в планировщике
	if report, parseErr := parser.Parse(post.Text, b.cfg.DefaultIssue, date); parseErr == nil && report.DayTotal(date) > 0 {
		return nil
	}
	message, err := b.send(ctx, fmt.Sprintf("📝 В посте недели отсутствуют записи за сегодня (%s %s)",
		strings.ToLower(weekdays[date.Weekday()]), date.Format("02.01")), statusKeyboard(postURL(post.ChatID, post.MessageID)))
	if err != nil {
		return err
	}
	if err := b.store.SaveDailyReminderMessage(context.WithoutCancel(ctx), b.userID, date, message.ID); err != nil {
		if b.retireMessage(ctx, message.ID, "Напоминание будет отправлено повторно") {
			return err
		}
		slog.Error("save daily reminder message", "error", err)
	}
	return nil
}

// ReconcileDailyReminders повторяет уборку после сбоя или перезапуска бота
func (b *Bot) ReconcileDailyReminders(ctx context.Context) {
	b.postMu.Lock()
	defer b.postMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	now := b.now()
	week := calendar.WeekStart(now)
	for _, start := range []time.Time{week, week.AddDate(0, 0, -7)} {
		reminders, err := b.store.DailyReminderMessages(ctx, b.userID, start, start.AddDate(0, 0, 6))
		if err != nil {
			slog.Warn("load daily reminder messages", "error", err)
			return
		}
		if len(reminders) == 0 {
			continue
		}
		post, err := b.store.WeekPostForWeek(ctx, b.userID, start)
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				slog.Warn("load week for daily reminders", "error", err)
			}
			continue
		}
		if report, err := parser.Parse(post.Text, b.cfg.DefaultIssue, now); err == nil {
			b.retireResolvedDailyReminders(ctx, report, reminders)
		}
	}
}

// removeResolvedDailyReminders убирает напоминания за заполненные даты, вызывать под postMu
func (b *Bot) removeResolvedDailyReminders(ctx context.Context, report parser.Report, week time.Time) {
	reminders, err := b.store.DailyReminderMessages(ctx, b.userID, week, week.AddDate(0, 0, 6))
	if err != nil {
		slog.Warn("load daily reminder messages", "error", err)
		return
	}
	b.retireResolvedDailyReminders(ctx, report, reminders)
}

func (b *Bot) retireResolvedDailyReminders(ctx context.Context, report parser.Report, reminders []store.DailyReminderMessage) {
	for _, reminder := range reminders {
		date := time.Date(reminder.Date.Year(), reminder.Date.Month(), reminder.Date.Day(), 0, 0, 0, 0, b.cfg.Location)
		if report.DayTotal(date) == 0 {
			continue
		}
		if !b.retireMessage(ctx, reminder.MessageID, "Записи за "+date.Format("02.01")+" добавлены") {
			continue
		}
		if err := b.store.ClearDailyReminderMessage(context.WithoutCancel(ctx), b.userID, reminder); err != nil {
			slog.Warn("forget daily reminder message", "error", err)
		}
	}
}

// WeeklyPreview - напоминание notify: превью недели с кнопками
func (b *Bot) WeeklyPreview(ctx context.Context) error {
	return b.showPreview(ctx, true, "🗓 Конец рабочей недели, подготовлен предпросмотр перед отправкой")
}
