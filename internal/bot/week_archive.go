package bot

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"strconv"
	"strings"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/devWaylander/auto-time-report/internal/calendar"
	"github.com/devWaylander/auto-time-report/internal/store"
)

const (
	botButtonText      = "🤖 В бота"
	archiveButtonText  = "📋 Итог недели"
	archiveStartPrefix = "week_"
)

// syncWeekArchive сохраняет итог из подтверждённых записей и меняет кнопку поста под postMu
func (b *Bot) syncWeekArchive(ctx context.Context, post store.WeekPost, view weekView, msg *models.Message) {
	if post.ID == 0 || post.ChatID != b.cfg.ChannelID {
		return
	}
	if view.archiveKnown && view.archiveText != post.ArchiveText {
		if err := b.store.SaveWeekArchive(ctx, post.ID, view.archiveText); err != nil {
			slog.Error("save week archive", "error", err)
			return
		}
		post.ArchiveText = view.archiveText
	}
	b.syncBotButton(ctx, post, msg)
}

func (b *Bot) syncBotButton(ctx context.Context, post store.WeekPost, msg *models.Message) {
	archived := post.ArchiveText != ""
	if msg == nil && (post.BotButtonBase == "" || post.BotButtonArchived == archived) {
		return
	}
	b.mu.Lock()
	off := b.buttonOff
	b.mu.Unlock()
	if off {
		return
	}
	link, err := b.botLink(ctx)
	if err != nil {
		slog.Warn("bot link for post button", "error", err)
		return
	}
	archiveLink := link + "?start=" + archiveStartPrefix + strconv.FormatInt(post.ID, 10)
	button := models.InlineKeyboardButton{Text: botButtonText, URL: link}
	if archived {
		button = models.InlineKeyboardButton{Text: archiveButtonText, URL: archiveLink}
	}

	base := models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{}}
	var hasWorkButton, hasArchiveButton bool
	if msg != nil && msg.ReplyMarkup != nil {
		base, hasWorkButton, hasArchiveButton = stripWeekButtons(msg.ReplyMarkup, link, archiveLink)
	} else if post.BotButtonBase != "" {
		if err := json.Unmarshal([]byte(post.BotButtonBase), &base); err != nil {
			slog.Warn("decode channel keyboard", "error", err)
			return
		}
	}
	encoded, err := json.Marshal(base)
	if err != nil {
		slog.Warn("encode channel keyboard", "error", err)
		return
	}
	if hasArchiveButton != archived || hasWorkButton == archived {
		markup := models.InlineKeyboardMarkup{
			InlineKeyboard: append(base.InlineKeyboard, []models.InlineKeyboardButton{button}),
		}
		if err := b.editChannelMarkup(ctx, post.ChatID, post.MessageID, markup); err != nil {
			slog.Warn("update bot button under week post", "error", err)
			if strings.Contains(err.Error(), "message can't be edited") || strings.Contains(err.Error(), "not enough rights") {
				b.mu.Lock()
				b.buttonOff = true
				b.mu.Unlock()
			}
			return
		}
	}
	if string(encoded) != post.BotButtonBase || post.BotButtonArchived != archived {
		if err := b.store.SetChannelBotButton(ctx, post.ID, string(encoded), archived); err != nil {
			slog.Warn("remember channel button", "error", err)
			return
		}
	}
	if b.isCurrentWeek(post) {
		b.removeOldWorkingButtons(ctx, post)
	}
}

// stripWeekButtons сохраняет соседние кнопки и порядок строк
func stripWeekButtons(markup *models.InlineKeyboardMarkup, link, archiveLink string) (models.InlineKeyboardMarkup, bool, bool) {
	base := models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{}}
	var hasWorkButton, hasArchiveButton bool
	for _, row := range markup.InlineKeyboard {
		var buttons []models.InlineKeyboardButton
		for _, button := range row {
			switch {
			case button.Text == botButtonText && button.URL == link:
				hasWorkButton = true
			case button.Text == archiveButtonText && button.URL == archiveLink:
				hasArchiveButton = true
			default:
				buttons = append(buttons, button)
			}
		}
		if len(buttons) > 0 {
			base.InlineKeyboard = append(base.InlineKeyboard, buttons)
		}
	}
	return base, hasWorkButton, hasArchiveButton
}

func (b *Bot) removeOldWorkingButtons(ctx context.Context, current store.WeekPost) {
	buttons, err := b.store.WorkingChannelBotButtons(ctx, b.userID, current.ChatID, calendar.WeekStart(b.now()))
	if err != nil {
		slog.Warn("load working channel buttons", "error", err)
		return
	}
	for _, button := range buttons {
		var base models.InlineKeyboardMarkup
		if err := json.Unmarshal([]byte(button.Base), &base); err != nil {
			slog.Warn("decode channel keyboard", "error", err)
			continue
		}
		err := b.editChannelMarkup(ctx, button.ChatID, button.MessageID, base)
		if err != nil && !strings.Contains(err.Error(), "message to edit not found") {
			slog.Warn("remove bot button from old post", "error", err)
			continue
		}
		if err := b.store.SetChannelBotButton(ctx, button.PostID, "", false); err != nil {
			slog.Warn("forget old channel button", "error", err)
		}
	}
}

func (b *Bot) editChannelMarkup(ctx context.Context, chatID, messageID int64, markup models.InlineKeyboardMarkup) error {
	_, err := b.api.EditMessageReplyMarkup(ctx, &tgbot.EditMessageReplyMarkupParams{
		ChatID: chatID, MessageID: int(messageID), ReplyMarkup: markup,
	})
	if err != nil && !strings.Contains(err.Error(), "message is not modified") {
		return errors.New(b.redact(err.Error()))
	}
	return nil
}

// sendWeekArchive открывает сохранённый итог конкретного поста из лички владельца
func (b *Bot) sendWeekArchive(ctx context.Context, msg *models.Message, id string) {
	postID, err := strconv.ParseInt(id, 10, 64)
	if err != nil || postID <= 0 {
		b.notify(ctx, "Итог недели недоступен")
		return
	}
	b.postMu.Lock()
	defer b.postMu.Unlock()

	archive, err := b.store.WeekArchive(ctx, b.userID, b.cfg.ChannelID, postID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.Error("load week archive", "error", err)
		}
		b.notify(ctx, "Итог недели недоступен")
		return
	}
	row := append(postButton(postURL(b.cfg.ChannelID, archive.ChannelMessageID)),
		models.InlineKeyboardButton{Text: "✖️ Скрыть", CallbackData: hideData})
	text := formatWeekArchive(archive.Text)
	markup := models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{row}}
	if _, _, _, ok := splitWeekTable(text); ok {
		markup = b.miniAppKeyboard(markup, postID, false, true)
	}
	card, err := b.send(ctx, text, markup)
	if err != nil {
		slog.Error("send week archive", "error", err)
		return
	}
	if err := b.store.SaveArchiveMessage(context.WithoutCancel(ctx), postID, card.ID); err != nil {
		slog.Error("save archive message", "error", err)
		b.retireMessage(ctx, card.ID, "Итог недели не сохранён")
		return
	}
	if archive.MessageID != 0 && archive.MessageID != card.ID {
		b.retireMessage(ctx, archive.MessageID, "Итог недели обновлён")
	}
	if err := b.deleteMessage(ctx, msg.Chat.ID, msg.ID); err != nil {
		slog.Warn("delete archive command", "error", err)
	}
}

// В старых итогах значок стоит после даты и необязательного времени
var archivedDay = regexp.MustCompile(`^((?:Вс|Пн|Вт|Ср|Чт|Пт|Сб) [0-9]{2}\.[0-9]{2}) +(?:([0-9].*?) +)?(` + weekStatusEmoji + `)(?: +(.*))?$`)

// formatWeekArchive обновляет таблицу дней в сохранённых итогах
func formatWeekArchive(text string) string {
	for _, tags := range [][2]string{{"<pre>", "</pre>"}, {"<blockquote><code>", "</code></blockquote>"}} {
		header, rest, ok := strings.Cut(text, tags[0])
		if !ok {
			continue
		}
		days, summary, ok := strings.Cut(rest, tags[1])
		if !ok {
			continue
		}
		lines := strings.Split(strings.Trim(days, "\r\n"), "\n")
		var rows strings.Builder
		for _, line := range lines {
			if parts := archivedDay.FindStringSubmatch(strings.TrimSpace(line)); parts != nil {
				total, norm, hasNorm := strings.Cut(parts[2], " из ")
				status, notes := parts[3], parts[4]
				if hasNorm {
					if notes != "" {
						notes = " · " + notes
					}
					notes = "норма " + norm + notes
				}
				if notes != "" {
					status += " " + notes
				}
				rows.WriteString(renderDayRow(parts[1], total, status))
			} else if issues, ok := strings.CutPrefix(line, "    "); ok {
				rows.WriteString(renderDayIssues(strings.TrimSpace(issues)))
			} else {
				return text
			}
		}
		return header + weekDaysOpen + rows.String() + weekDaysClose + summary
	}
	return text
}
