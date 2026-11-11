// Package parser разбирает недельный пост с отработанным временем. Формат описан в README
package parser

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Entry - одна строка записи из поста
type Entry struct {
	Line     int
	Text     string
	Duration time.Duration
}

// Worklog - все записи одной задачи за один день, в Jira уходит одним worklog'ом
type Worklog struct {
	IssueKey string
	Date     time.Time
	Entries  []Entry
}

// Duration - суммарное время worklog'а
func (w Worklog) Duration() time.Duration {
	var total time.Duration
	for _, e := range w.Entries {
		total += e.Duration
	}
	return total
}

// Comment - строки записей как в посте, через пустую строку
func (w Worklog) Comment() string {
	lines := make([]string, len(w.Entries))
	for i, e := range w.Entries {
		lines[i] = e.Text
	}
	return strings.Join(lines, "\n\n")
}

// Report - разобранный пост, worklog'и отсортированы по дате, затем по задаче
type Report struct {
	Worklogs []Worklog
}

// Dates возвращает даты из поста по возрастанию, без повторов
func (r Report) Dates() []time.Time {
	var dates []time.Time
	for _, w := range r.Worklogs {
		if len(dates) == 0 || !dates[len(dates)-1].Equal(w.Date) {
			dates = append(dates, w.Date)
		}
	}
	return dates
}

// DayTotal - сумма по всем задачам за дату
func (r Report) DayTotal(date time.Time) time.Duration {
	var total time.Duration
	for _, w := range r.Worklogs {
		if w.Date.Equal(date) {
			total += w.Duration()
		}
	}
	return total
}

// Total - сумма за весь пост
func (r Report) Total() time.Duration {
	var total time.Duration
	for _, w := range r.Worklogs {
		total += w.Duration()
	}
	return total
}

// LineError - ошибка формата в конкретной строке поста. Человеку показываются день и начало строки:
// номер в Telegram не сверить - длинные строки переносятся, экраны разные
type LineError struct {
	Line int       // номер строки с единицы, считая пустые; нужен только для порядка ошибок
	Date time.Time // день, под которым стоит строка; нулевой - строка до первой даты или сама строка-дата
	Text string    // строка поста; пусто - ошибка про день целиком
	Msg  string
}

// Сколько символов строки показывать в ошибке
const shownLineRunes = 50

func (e *LineError) Error() string {
	var where []string
	if !e.Date.IsZero() {
		where = append(where, e.Date.Format("02.01"))
	}
	if e.Text != "" {
		text := e.Text
		if r := []rune(text); len(r) > shownLineRunes {
			// Режем по последнему пробелу, чтобы не обрывать слово
			cut := string(r[:shownLineRunes])
			if i := strings.LastIndex(cut, " "); i > 0 {
				cut = cut[:i]
			}
			text = strings.TrimRight(cut, " ,.-:;") + "…"
		}
		where = append(where, `"`+text+`"`)
	}
	if len(where) == 0 {
		return e.Msg
	}
	return strings.Join(where, ", ") + ": " + e.Msg
}

const maxDayDuration = 24 * time.Hour

var (
	issueLineRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]*-[0-9]+$`)
	dateLineRe  = regexp.MustCompile(`^([0-9]{1,2})\.([0-9]{1,2})$`)
)

// Parse разбирает пост. defaultIssue используется, пока в посте не встретилась строка-задача.
// now задаёт "сегодня" и часовой пояс дат: передавай время в часовом поясе бота.
// Ошибки формата возвращаются все сразу, каждая как *LineError
func Parse(text, defaultIssue string, now time.Time) (Report, error) {
	var (
		errs     []error
		worklogs = map[worklogKey]*Worklog{}
		// Пары "задача + дата", для которых дата уже встречалась: повтор - ошибка
		seenDates = map[worklogKey]bool{}
		issue     = defaultIssue
		// Нулевая дата - даты ещё нет или задача только что сменилась
		date time.Time
		// Дата в строке битая: записи под ней пропускаем молча, ошибка уже показана
		badDate bool
		today   = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	)
	fail := func(line int, date time.Time, text, format string, args ...any) {
		errs = append(errs, &LineError{Line: line, Date: date, Text: text, Msg: fmt.Sprintf(format, args...)})
	}

	for i, raw := range strings.Split(text, "\n") {
		n := i + 1
		line := normalizeLine(raw)
		switch {
		case line == "":
			continue

		case issueLineRe.MatchString(line):
			issue = line
			// После смены задачи дата должна быть указана заново, чтобы записи не уехали не туда
			date, badDate = time.Time{}, false

		case dateLineRe.MatchString(line):
			d, err := resolveDate(dateLineRe.FindStringSubmatch(line), today)
			if err != nil {
				fail(n, time.Time{}, line, "%v", err)
				date, badDate = time.Time{}, true
				continue
			}
			key := worklogKey{issue: issue, date: d}
			if seenDates[key] {
				fail(n, time.Time{}, line, "дата %s для задачи %s уже была выше", d.Format("02.01"), issue)
				date, badDate = time.Time{}, true
				continue
			}
			seenDates[key] = true
			date, badDate = d, false

		default:
			if badDate {
				continue
			}
			if date.IsZero() {
				fail(n, date, line, "запись без даты: перед ней должна быть строка вида 19.10")
				continue
			}
			if issue == "" {
				fail(n, date, line, "не указана задача: нужна строка с ключом задачи или JIRA_DEFAULT_ISSUE_KEY")
				continue
			}
			d, err := parseDuration(line)
			if err != nil {
				fail(n, date, line, "%v", err)
				continue
			}
			key := worklogKey{issue: issue, date: date}
			w, ok := worklogs[key]
			if !ok {
				w = &Worklog{IssueKey: issue, Date: date}
				worklogs[key] = w
			}
			w.Entries = append(w.Entries, Entry{Line: n, Text: line, Duration: d})
		}
	}

	report := Report{Worklogs: make([]Worklog, 0, len(worklogs))}
	for _, w := range worklogs {
		report.Worklogs = append(report.Worklogs, *w)
	}
	sort.Slice(report.Worklogs, func(i, j int) bool {
		a, b := report.Worklogs[i], report.Worklogs[j]
		if !a.Date.Equal(b.Date) {
			return a.Date.Before(b.Date)
		}
		return a.IssueKey < b.IssueKey
	})

	for _, d := range report.Dates() {
		if total := report.DayTotal(d); total > maxDayDuration {
			fail(firstLineOf(report, d), d, "", "за день набралось %s, это больше суток", FormatDuration(total))
		}
	}

	if len(errs) > 0 {
		sort.SliceStable(errs, func(i, j int) bool {
			return lineOf(errs[i]) < lineOf(errs[j])
		})
		return Report{}, errors.Join(errs...)
	}
	if len(report.Worklogs) == 0 {
		return Report{}, errors.New("в посте нет ни одной записи")
	}
	return report, nil
}

// LooksLikePost - похоже ли сообщение на пост с временем: первая непустая строка - дата или ключ задачи.
// Нужно, чтобы бот не принимал за пост недели другие заметки в канале
func LooksLikePost(text string) bool {
	for raw := range strings.SplitSeq(text, "\n") {
		line := normalizeLine(raw)
		if line == "" {
			continue
		}
		return dateLineRe.MatchString(line) || issueLineRe.MatchString(line)
	}
	return false
}

// normalizeLine убирает пробелы по краям, неразрывные пробелы и BOM, который добавляют некоторые клиенты
func normalizeLine(raw string) string {
	return strings.TrimSpace(strings.NewReplacer("\u00a0", " ", "\ufeff", "").Replace(raw))
}

type worklogKey struct {
	issue string
	date  time.Time
}

func lineOf(err error) int {
	if le, ok := errors.AsType[*LineError](err); ok {
		return le.Line
	}
	return 0
}

func firstLineOf(r Report, date time.Time) int {
	line := math.MaxInt
	for _, w := range r.Worklogs {
		if w.Date.Equal(date) && len(w.Entries) > 0 && w.Entries[0].Line < line {
			line = w.Entries[0].Line
		}
	}
	return line
}

// resolveDate подбирает год, при котором дата ближе всего к сегодня; будущие даты - ошибка
func resolveDate(m []string, today time.Time) (time.Time, error) {
	day, _ := strconv.Atoi(m[1])
	month, _ := strconv.Atoi(m[2])
	label := fmt.Sprintf("%02d.%02d", day, month)
	if month < 1 || month > 12 || day < 1 || day > 31 {
		return time.Time{}, fmt.Errorf("дата %s не существует", label)
	}

	var (
		best     time.Time
		bestDist time.Duration = math.MaxInt64
	)
	for _, y := range []int{today.Year() - 1, today.Year(), today.Year() + 1} {
		d := time.Date(y, time.Month(month), day, 0, 0, 0, 0, today.Location())
		// time.Date нормализует 31.02 в март: такая дата в этом году не существует
		if d.Day() != day || int(d.Month()) != month {
			continue
		}
		dist := d.Sub(today)
		if dist < 0 {
			dist = -dist
		}
		if dist < bestDist {
			best, bestDist = d, dist
		}
	}
	if best.IsZero() {
		return time.Time{}, fmt.Errorf("дата %s не существует", label)
	}
	if best.After(today) {
		return time.Time{}, fmt.Errorf("дата %s в будущем", best.Format("02.01.2006"))
	}
	return best, nil
}

// Время записи: "8ч", "30м" или "8ч 30м", слитно с буквой и в самом конце строки после последнего " - "
var durationRe = regexp.MustCompile(`^(?:([0-9]+)ч)?\s*(?:([0-9]+)м)?$`)

// Потолок одной записи: защищает и от опечаток, и от переполнения
const maxEntryMinutes = 24 * 60

var (
	// В строке нет " - <время>"
	errNoDuration = errors.New(`не найдено время, ожидается формат: "XXч XXм", "XXч", "XXм"`)
	// После последнего " - " стоит не время
	errDurationFormat = errors.New(`ожидается формат времени: "XXч XXм", "XXч", "XXм"`)
)

// parseDuration берёт время из конца строки: запись имеет вид "<описание> - <время>". Всё до последнего
// " - " - описание, в нём могут быть любые числа, версии и ключи задач
func parseDuration(line string) (time.Duration, error) {
	i := strings.LastIndex(line, " - ")
	if i < 0 || strings.TrimSpace(line[:i]) == "" {
		return 0, errNoDuration
	}
	tail := strings.TrimSpace(line[i+len(" - "):])
	if tail == "" {
		return 0, errNoDuration
	}
	m := durationRe.FindStringSubmatch(strings.ToLower(tail))
	if m == nil {
		return 0, errDurationFormat
	}

	hours, _ := strconv.Atoi(m[1])
	minutes, _ := strconv.Atoi(m[2])
	if m[1] != "" && minutes > 59 {
		return 0, errors.New("минут должно быть меньше 60")
	}
	total := hours*60 + minutes
	switch {
	case len(m[1]) > 4 || len(m[2]) > 4 || total > maxEntryMinutes:
		return 0, errors.New("в одной записи больше суток")
	case total == 0:
		return 0, errors.New("время должно быть больше нуля")
	}
	return time.Duration(total) * time.Minute, nil
}

// FormatDuration печатает длительность по-человечески: "7ч 30м", "8ч", "45м"
func FormatDuration(d time.Duration) string {
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	switch {
	case h > 0 && m > 0:
		return fmt.Sprintf("%dч %dм", h, m)
	case h > 0:
		return fmt.Sprintf("%dч", h)
	default:
		return fmt.Sprintf("%dм", m)
	}
}
