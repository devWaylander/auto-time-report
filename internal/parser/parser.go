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
	"unicode"
	"unicode/utf8"
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

// LineError - ошибка формата в конкретной строке поста
type LineError struct {
	Line int
	Msg  string
}

func (e *LineError) Error() string {
	return fmt.Sprintf("строка %d: %s", e.Line, e.Msg)
}

const maxDayDuration = 24 * time.Hour

var (
	issueLineRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]*-[0-9]+$`)
	dateLineRe  = regexp.MustCompile(`^([0-9]{1,2})\.([0-9]{1,2})$`)
	// Число и слово сразу за ним: "2 часа", "30мин", "1,5 ч"
	amountRe = regexp.MustCompile(`([0-9]+(?:[.,][0-9]+)?)\s*([\p{L}]+)`)
)

// Parse разбирает пост. defaultIssue используется, пока в посте не встретилась строка-задача.
// now задаёт "сегодня" и часовой пояс дат: передавай время в часовом поясе бота.
// Ошибки формата возвращаются все сразу, каждая как *LineError
func Parse(text, defaultIssue string, now time.Time) (Report, error) {
	var (
		errs     []error
		worklogs = map[worklogKey]*Worklog{}
		// Первая строка, где для пары "задача + дата" встретилась дата, для сообщения о повторе
		dateLines = map[worklogKey]int{}
		issue     = defaultIssue
		// Нулевая дата - даты ещё нет или задача только что сменилась
		date time.Time
		// Дата в строке битая: записи под ней пропускаем молча, ошибка уже показана
		badDate bool
		today   = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	)
	fail := func(line int, format string, args ...any) {
		errs = append(errs, &LineError{Line: line, Msg: fmt.Sprintf(format, args...)})
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
				fail(n, "%v", err)
				date, badDate = time.Time{}, true
				continue
			}
			key := worklogKey{issue: issue, date: d}
			if first, ok := dateLines[key]; ok {
				fail(n, "дата %s для задачи %s уже была в строке %d", d.Format("02.01"), issue, first)
				date, badDate = time.Time{}, true
				continue
			}
			dateLines[key] = n
			date, badDate = d, false

		default:
			if badDate {
				continue
			}
			if date.IsZero() {
				fail(n, "запись без даты: перед ней должна быть строка вида 19.10")
				continue
			}
			if issue == "" {
				fail(n, "не указана задача: добавь строку с ключом задачи или задай JIRA_DEFAULT_ISSUE_KEY")
				continue
			}
			d, err := parseDuration(line)
			if err != nil {
				fail(n, "%v", err)
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
			fail(firstLineOf(report, d), "за %s набралось %s, это больше суток", d.Format("02.01"), FormatDuration(total))
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
	for _, raw := range strings.Split(text, "\n") {
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
	var le *LineError
	if errors.As(err, &le) {
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

var (
	hourWords   = map[string]bool{"ч": true, "час": true, "часа": true, "часов": true}
	minuteWords = map[string]bool{"мин": true, "минута": true, "минуту": true, "минуты": true, "минут": true}
)

// Потолок одной записи: защищает и от опечаток, и от переполнения при умножении
const maxEntryMinutes = 24 * 60

// parseDuration ищет в строке "N час/часа/часов/ч" и "N мин/минут…". Единицы времени - только точные слова,
// чтобы "5 минимальных" или "12 часовой" не стали временем. Число не считается длительностью, если оно часть
// ключа задачи ("ABC-123 чинил") или времени ("10:30 часов")
func parseDuration(line string) (time.Duration, error) {
	var hours, minutes []float64
	for _, m := range amountRe.FindAllStringSubmatchIndex(line, -1) {
		if partOfToken(line[:m[0]]) {
			continue
		}
		num, err := strconv.ParseFloat(strings.Replace(line[m[2]:m[3]], ",", ".", 1), 64)
		if err != nil {
			continue
		}
		word := strings.ToLower(line[m[4]:m[5]])
		switch {
		case hourWords[word]:
			hours = append(hours, num)
		case minuteWords[word]:
			minutes = append(minutes, num)
		}
	}

	switch {
	case len(hours) == 0 && len(minutes) == 0:
		return 0, errors.New(`не нашёл длительность: нужно, например, "2 часа" или "30 минут"`)
	case len(hours) > 1:
		return 0, errors.New("часы указаны несколько раз")
	case len(minutes) > 1:
		return 0, errors.New("минуты указаны несколько раз")
	}

	var total float64
	if len(hours) == 1 {
		total += hours[0] * 60
	}
	if len(minutes) == 1 {
		total += minutes[0]
	}
	if total > maxEntryMinutes {
		return 0, errors.New("в одной записи больше суток")
	}
	d := time.Duration(math.Round(total)) * time.Minute
	if d <= 0 {
		return 0, errors.New("длительность должна быть больше нуля")
	}
	return d, nil
}

// partOfToken - число продолжает слово, ключ задачи или время: перед ним ASCII-буква, цифра или "_",
// либо "-", ".", ",", ":" сразу после ASCII-буквы или цифры ("ABC-123", "v1.2", "10:30")
func partOfToken(before string) bool {
	prev, size := utf8.DecodeLastRuneInString(before)
	if size == 0 {
		return false
	}
	if isASCIIAlnum(prev) || prev == '_' {
		return true
	}
	if prev == '-' || prev == '.' || prev == ',' || prev == ':' {
		prev2, _ := utf8.DecodeLastRuneInString(before[:len(before)-size])
		return isASCIIAlnum(prev2)
	}
	return false
}

func isASCIIAlnum(r rune) bool {
	return r < utf8.RuneSelf && (unicode.IsLetter(r) || unicode.IsDigit(r))
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
