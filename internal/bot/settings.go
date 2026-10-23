package bot

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-telegram/bot/models"

	"github.com/devWaylander/auto-time-report/internal/store"
)

const settingsPrefix = "set:"

const (
	setDaily   = settingsPrefix + "daily"
	setWeekly  = settingsPrefix + "weekly"
	setPause1  = settingsPrefix + "p1"
	setPause7  = settingsPrefix + "p7"
	setUnpause = settingsPrefix + "p0"
)

var hhmmRe = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)

var weekdayNames = map[string]time.Weekday{
	"пн": time.Monday, "вт": time.Tuesday, "ср": time.Wednesday, "чт": time.Thursday,
	"пт": time.Friday, "сб": time.Saturday, "вс": time.Sunday,
}

func (b *Bot) sendSettings(ctx context.Context) {
	text, markup, err := b.settingsView(ctx)
	if err != nil {
		slog.Error("settings", "error", err)
		b.notify(ctx, "⚠️ Не смог прочитать настройки, загляни в логи бота.")
		return
	}
	if _, err := b.send(ctx, text, markup); err != nil {
		slog.Error("send settings", "error", err)
	}
}

func (b *Bot) settingsView(ctx context.Context) (string, models.ReplyMarkup, error) {
	st, err := b.store.Settings(ctx, b.userID)
	if err != nil {
		return "", nil, err
	}
	vacations, err := b.store.UpcomingVacations(ctx, b.userID, b.today())
	if err != nil {
		return "", nil, err
	}
	return renderSettings(st, vacations, b.now()), settingsKeyboard(st, b.now()), nil
}

func renderSettings(st store.Settings, vacations []store.Vacation, now time.Time) string {
	onOff := func(v bool) string {
		if v {
			return "вкл"
		}
		return "выкл"
	}
	var b strings.Builder
	b.WriteString("<b>Напоминания</b>\n\n")
	fmt.Fprintf(&b, "🔔 Ежедневное: %s · %s\n", onOff(st.DailyEnabled), st.DailyTime)
	fmt.Fprintf(&b, "📅 Недельное превью: %s · %s %s\n", onOff(st.WeeklyEnabled),
		strings.ToLower(weekdays[st.WeeklyWeekday]), st.WeeklyTime)
	if now.Before(st.PausedUntil) {
		fmt.Fprintf(&b, "🔕 Пауза до %s\n", st.PausedUntil.In(now.Location()).Format("02.01 15:04"))
	} else {
		b.WriteString("🔕 Паузы нет\n")
	}
	if len(vacations) == 0 {
		b.WriteString("🏖 Отпусков нет\n")
	}
	for _, v := range vacations {
		fmt.Fprintf(&b, "🏖 Отпуск %s-%s\n", v.Start.Format("02.01"), v.End.Format("02.01.2006"))
	}
	b.WriteString("\nВыходные и праздники по производственному календарю пропускаются сами.\n" +
		"Время: /daily 19:30, /weekly пт 16:00. Отпуск: /vacation 27.10-31.10, /vacation off")
	return b.String()
}

func settingsKeyboard(st store.Settings, now time.Time) models.ReplyMarkup {
	toggle := func(name string, on bool) string {
		if on {
			return "Выключить " + name
		}
		return "Включить " + name
	}
	pause := []models.InlineKeyboardButton{
		{Text: "Пауза до завтра", CallbackData: setPause1},
		{Text: "Пауза на неделю", CallbackData: setPause7},
	}
	if now.Before(st.PausedUntil) {
		pause = []models.InlineKeyboardButton{{Text: "Снять паузу", CallbackData: setUnpause}}
	}
	return models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{
		{{Text: toggle("ежедневное", st.DailyEnabled), CallbackData: setDaily}},
		{{Text: toggle("недельное", st.WeeklyEnabled), CallbackData: setWeekly}},
		pause,
	}}
}

func (b *Bot) onSettingsButton(ctx context.Context, cq *models.CallbackQuery, messageID int) {
	err := b.updateSettings(ctx, func(st *store.Settings) {
		today := b.today()
		switch cq.Data {
		case setDaily:
			st.DailyEnabled = !st.DailyEnabled
		case setWeekly:
			st.WeeklyEnabled = !st.WeeklyEnabled
		case setPause1:
			st.PausedUntil = today.AddDate(0, 0, 1)
		case setPause7:
			st.PausedUntil = today.AddDate(0, 0, 7)
		case setUnpause:
			st.PausedUntil = time.Time{}
		}
	})
	if err != nil {
		slog.Error("update settings", "error", err)
		b.answer(ctx, cq, "Не смог сохранить", true)
		return
	}
	b.answer(ctx, cq, "Сохранил", false)

	text, markup, err := b.settingsView(ctx)
	if err != nil {
		slog.Error("settings", "error", err)
		return
	}
	if messageID != 0 {
		b.edit(ctx, messageID, text, markup)
	}
}

func (b *Bot) updateSettings(ctx context.Context, change func(*store.Settings)) error {
	st, err := b.store.Settings(ctx, b.userID)
	if err != nil {
		return err
	}
	change(&st)
	return b.store.UpdateSettings(ctx, st)
}

func (b *Bot) cmdDaily(ctx context.Context, args string) {
	if !hhmmRe.MatchString(args) {
		b.notify(ctx, "Формат: /daily 19:30")
		return
	}
	if err := b.updateSettings(ctx, func(st *store.Settings) {
		st.DailyTime, st.DailyEnabled = args, true
	}); err != nil {
		slog.Error("update settings", "error", err)
		b.notify(ctx, "⚠️ Не смог сохранить настройки.")
		return
	}
	b.notify(ctx, "🔔 Ежедневное напоминание в "+args+" по рабочим дням, если за день нет записей.")
}

func (b *Bot) cmdWeekly(ctx context.Context, args string) {
	day, hhmm, _ := strings.Cut(args, " ")
	weekday, ok := weekdayNames[strings.ToLower(strings.TrimSpace(day))]
	hhmm = strings.TrimSpace(hhmm)
	if !ok || !hhmmRe.MatchString(hhmm) {
		b.notify(ctx, "Формат: /weekly пт 16:00 (дни: пн вт ср чт пт сб вс)")
		return
	}
	if err := b.updateSettings(ctx, func(st *store.Settings) {
		st.WeeklyWeekday, st.WeeklyTime, st.WeeklyEnabled = weekday, hhmm, true
	}); err != nil {
		slog.Error("update settings", "error", err)
		b.notify(ctx, "⚠️ Не смог сохранить настройки.")
		return
	}
	b.notify(ctx, fmt.Sprintf("📅 Недельное превью: %s в %s.", strings.ToLower(weekdays[weekday]), hhmm))
}

func (b *Bot) cmdVacation(ctx context.Context, args string) {
	today := b.today()
	if args == "off" {
		if err := b.store.CancelVacations(ctx, b.userID, today); err != nil {
			slog.Error("cancel vacations", "error", err)
			b.notify(ctx, "⚠️ Не смог отменить отпуск.")
			return
		}
		b.notify(ctx, "🏖 Текущие и будущие отпуска отменены, напоминания снова идут.")
		return
	}

	start, end, err := parseVacation(args, today)
	if err != nil {
		b.notify(ctx, "Формат: /vacation 27.10-31.10 ("+err.Error()+")")
		return
	}
	if _, err := b.store.AddVacation(ctx, b.userID, start, end); err != nil {
		slog.Error("add vacation", "error", err)
		b.notify(ctx, "⚠️ Не смог сохранить отпуск.")
		return
	}
	b.notify(ctx, fmt.Sprintf("🏖 Отпуск %s-%s: напоминаний не будет.", start.Format("02.01.2006"), end.Format("02.01.2006")))
}

// parseVacation разбирает "ДД.ММ-ДД.ММ". Год - ближайший, при котором отпуск ещё не закончился:
// в декабре "05.01-10.01" значит январь следующего года
func parseVacation(s string, today time.Time) (time.Time, time.Time, error) {
	from, to, ok := strings.Cut(strings.ReplaceAll(s, " ", ""), "-")
	if !ok {
		return time.Time{}, time.Time{}, fmt.Errorf("нужен диапазон")
	}
	start, err := parseDayMonth(from, today.Year(), today.Location())
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	end, err := parseDayMonth(to, today.Year(), today.Location())
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if end.Before(start) {
		end = end.AddDate(1, 0, 0)
	}
	if end.Before(today) {
		start, end = start.AddDate(1, 0, 0), end.AddDate(1, 0, 0)
	}
	if end.Sub(start) > 92*24*time.Hour {
		return time.Time{}, time.Time{}, fmt.Errorf("слишком длинный отпуск")
	}
	return start, end, nil
}

func parseDayMonth(s string, year int, loc *time.Location) (time.Time, error) {
	d, m, ok := strings.Cut(s, ".")
	day, err1 := strconv.Atoi(d)
	month, err2 := strconv.Atoi(m)
	if !ok || err1 != nil || err2 != nil {
		return time.Time{}, fmt.Errorf("не понял дату %q", s)
	}
	t := time.Date(year, time.Month(month), day, 0, 0, 0, 0, loc)
	if t.Day() != day || int(t.Month()) != month {
		return time.Time{}, fmt.Errorf("даты %q не существует", s)
	}
	return t, nil
}
