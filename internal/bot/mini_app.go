package bot

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-telegram/bot/models"

	"github.com/devWaylander/auto-time-report/internal/store"
)

//go:embed mini_app.html
var miniAppHTML string

//go:embed week_card.html
var weekCardHTML string

var miniAppPage = template.Must(template.New("mini app").Parse(miniAppHTML))
var weekCard = template.Must(template.New("week card").Parse(weekCardHTML))

func (b *Bot) miniAppKeyboard(markup models.InlineKeyboardMarkup, postID int64, submit, archive bool) models.InlineKeyboardMarkup {
	if b.cfg.MiniAppURL == nil {
		return markup
	}
	u := *b.cfg.MiniAppURL
	query := url.Values{"post": {strconv.FormatInt(postID, 10)}}
	if submit {
		query.Set("submit", "1")
	}
	if archive {
		query.Set("archive", "1")
	}
	u.RawQuery = query.Encode()
	row := []models.InlineKeyboardButton{{Text: "Открыть превью", WebApp: &models.WebAppInfo{URL: u.String()}}}
	markup.InlineKeyboard = append([][]models.InlineKeyboardButton{row}, markup.InlineKeyboard...)
	return markup
}

// ServeMiniApp отдаёт страницу и загружает отчёт после проверки подписи Telegram
func (b *Bot) ServeMiniApp(w http.ResponseWriter, r *http.Request) {
	if b.cfg.MiniAppURL == nil || r.URL.Path != miniAppPath(b.cfg.MiniAppURL) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	switch r.Method {
	case http.MethodGet:
		nonce := rand.Text()
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src https://telegram.org 'nonce-"+nonce+"'; style-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := miniAppPage.Execute(w, nonce); err != nil {
			slog.Error("render mini app", "error", err)
		}
	case http.MethodPost:
		b.serveMiniAppCard(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func miniAppPath(u *url.URL) string {
	if u.Path == "" {
		return "/"
	}
	return u.Path
}

func (b *Bot) serveMiniAppCard(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<10))
	if err != nil {
		http.Error(w, "Не удалось проверить вход через Telegram", http.StatusBadRequest)
		return
	}
	if !validMiniAppOwner(string(data), b.cfg.Token.Reveal(), b.cfg.OwnerID, b.now()) {
		http.Error(w, "Открой превью заново из чата с ботом", http.StatusUnauthorized)
		return
	}
	query := r.URL.Query()
	postID, err := strconv.ParseInt(query.Get("post"), 10, 64)
	if err != nil || postID <= 0 {
		http.Error(w, "Предпросмотр недоступен", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	text, err := b.miniAppPreview(ctx, postID, query.Get("archive") == "1", query.Get("submit") == "1")
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "Предпросмотр устарел. Обнови статус в боте", http.StatusNotFound)
		return
	}
	if err != nil {
		slog.Error("load mini app preview", "error", err)
		http.Error(w, "Не удалось загрузить предпросмотр", http.StatusInternalServerError)
		return
	}
	var card bytes.Buffer
	if err := renderWeekCard(&card, text); err != nil {
		slog.Error("render mini app preview", "error", err)
		http.Error(w, "Не удалось показать предпросмотр", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(card.Bytes())
}

func (b *Bot) miniAppPreview(ctx context.Context, postID int64, archive, submit bool) (string, error) {
	if archive {
		week, err := b.store.WeekArchive(ctx, b.userID, b.cfg.ChannelID, postID)
		return formatWeekArchive(week.Text), err
	}
	post, err := b.currentWeekPost(ctx)
	if err != nil {
		return "", err
	}
	if post.ID != postID {
		return "", store.ErrNotFound
	}
	week, err := b.loadWeekOf(ctx, post)
	view := b.viewOf(ctx, week, err, submit, "")
	return view.text, nil
}

// renderWeekCard отдаёт самодостаточный HTML для Mini App и будущего виджета
func renderWeekCard(w io.Writer, text string) error {
	header, rows, footer, ok := splitWeekTable(text)
	if !ok {
		header = text
	}
	// Разметка приходит из рендерера бота или сохранённого им итога
	return weekCard.Execute(w, struct{ Header, Rows, Footer template.HTML }{
		Header: template.HTML(header), Rows: template.HTML(rows), Footer: template.HTML(footer),
	})
}

func validMiniAppOwner(initData, token string, ownerID int64, now time.Time) bool {
	values, err := url.ParseQuery(initData)
	if err != nil || token == "" || ownerID == 0 {
		return false
	}
	var fields []string
	for key, value := range values {
		if len(value) != 1 || strings.ContainsAny(key+value[0], "\r\n") {
			return false
		}
		if key != "hash" {
			fields = append(fields, key+"="+value[0])
		}
	}
	slices.Sort(fields)
	hash, err := hex.DecodeString(values.Get("hash"))
	if err != nil {
		return false
	}
	secret := hmac.New(sha256.New, []byte("WebAppData"))
	_, _ = secret.Write([]byte(token))
	mac := hmac.New(sha256.New, secret.Sum(nil))
	_, _ = mac.Write([]byte(strings.Join(fields, "\n")))
	if !hmac.Equal(hash, mac.Sum(nil)) {
		return false
	}
	authDate, err := strconv.ParseInt(values.Get("auth_date"), 10, 64)
	if err != nil || authDate < now.Add(-15*time.Minute).Unix() || authDate > now.Add(time.Minute).Unix() {
		return false
	}
	var user struct{ ID int64 }
	return json.Unmarshal([]byte(values.Get("user")), &user) == nil && user.ID == ownerID
}
