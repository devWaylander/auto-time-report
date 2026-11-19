package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-telegram/bot/models"
)

const widgetProbeHTML = `<style>
.days {
  display: grid;
  grid-template-columns: 4.4em 2.2em auto;
  gap: 3px 8px;
  padding: 6px 9px;
  border-left: 3px solid var(--tg-theme-link-color);
  border-radius: 5px;
  background: var(--tg-theme-secondary-bg-color);
  font-variant-numeric: tabular-nums;
}
</style>
<div class="days">
  <span>Пн 16.11</span><span>8ч</span><span>☑️ вручную</span>
  <span>Вт 17.11</span><span>8ч</span><span>🔹</span>
  <span>Ср 18.11</span><span>8ч</span><span>🔹</span>
  <span>Чт 19.11</span><span></span><span>▫️ сегодня</span>
  <span>Пт 20.11</span><span></span><span>▫️</span>
</div>`

func (b *Bot) testWidget(ctx context.Context, msg *models.Message) {
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	err := b.sendWidgetProbe(probeCtx)
	cancel()
	if err != nil {
		b.notify(ctx, "Ответ <code>sendWidget</code>\n<code>"+html.EscapeString(b.redact(err.Error()))+"</code>")
		return
	}
	if err := b.deleteMessage(ctx, msg.Chat.ID, msg.ID); err != nil {
		slog.Warn("delete widget test command", "error", err)
	}
}

// sendWidgetProbe проверяет новый метод Bot API на тестовых данных
func (b *Bot) sendWidgetProbe(ctx context.Context) error {
	baseURL := b.cfg.ServerURL
	if baseURL == "" {
		baseURL = "https://api.telegram.org"
	}
	form := url.Values{
		"chat_id":              {strconv.FormatInt(b.cfg.OwnerID, 10)},
		"html":                 {widgetProbeHTML},
		"width":                {"224"},
		"caption":              {"Тест HTML-виджета · 24ч из 40ч по норме (не хватает 16ч)"},
		"disable_notification": {"true"},
	}
	endpoint := strings.TrimRight(baseURL, "/") + "/bot" + b.cfg.Token.Reveal() + "/sendWidget"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	var result struct {
		OK          bool   `json:"ok"`
		ErrorCode   int    `json:"error_code"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("HTTP %d: %w", resp.StatusCode, err)
	}
	if !result.OK {
		return fmt.Errorf("%d %s", result.ErrorCode, result.Description)
	}
	return nil
}
