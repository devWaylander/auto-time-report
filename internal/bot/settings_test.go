package bot

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/go-telegram/bot/models"

	"github.com/devWaylander/auto-time-report/internal/store"
)

func TestSettingsReminderButtonsSetDisplayedValue(t *testing.T) {
	for _, tc := range []struct {
		name   string
		row    int
		legacy string
		value  func(store.Settings) bool
	}{
		{"ежедневное", 0, "set:daily", func(st store.Settings) bool { return st.DailyEnabled }},
		{"недельное", 1, "set:weekly", func(st store.Settings) bool { return st.WeeklyEnabled }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			b, tg, _, st := newTelegramBot(t)
			readButton := func(calls []apiCall, method string) models.InlineKeyboardButton {
				t.Helper()
				var markup models.InlineKeyboardMarkup
				if err := json.Unmarshal([]byte(lastParam(calls, method, "reply_markup")), &markup); err != nil {
					t.Fatal(err)
				}
				if len(markup.InlineKeyboard) <= tc.row || len(markup.InlineKeyboard[tc.row]) == 0 {
					t.Fatalf("settings button is missing: %+v", markup)
				}
				return markup.InlineKeyboard[tc.row][0]
			}
			press := func(id int, data string) []apiCall {
				b.onCallback(ctx, &models.CallbackQuery{
					ID: "btn", From: models.User{ID: b.cfg.OwnerID}, Data: data,
					Message: models.MaybeInaccessibleMessage{Message: &models.Message{
						ID: id, Chat: models.Chat{ID: b.cfg.OwnerID, Type: models.ChatTypePrivate},
					}},
				})
				return tg.take()
			}
			checkEnabled := func(want bool) {
				t.Helper()
				settings, err := st.Settings(ctx, b.userID)
				if err != nil || tc.value(settings) != want {
					t.Fatalf("reminder enabled = %t, want %t, err = %v", tc.value(settings), want, err)
				}
			}

			b.onMessage(ctx, dm("/settings"))
			older := readButton(tg.take(), "sendMessage")
			b.onMessage(ctx, dm("/settings"))
			newer := readButton(tg.take(), "sendMessage")
			if older.Text != "Выключить "+tc.name || newer.Text != older.Text {
				t.Fatalf("expected two disable buttons: %+v and %+v", older, newer)
			}
			press(103, newer.CallbackData)
			checkEnabled(false)
			press(102, older.CallbackData)
			checkEnabled(false)

			calls := press(101, tc.legacy)
			checkEnabled(false)
			if got := lastParam(calls, "answerCallbackQuery", "text"); got != "Кнопка устарела" {
				t.Fatalf("legacy button response = %q", got)
			}
			enable := readButton(calls, "editMessageText")
			if enable.Text != "Включить "+tc.name {
				t.Fatalf("legacy card was not updated: %+v", enable)
			}
			press(101, enable.CallbackData)
			press(101, enable.CallbackData)
			checkEnabled(true)
		})
	}
}
