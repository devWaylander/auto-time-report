// Package config читает настройки бота из переменных окружения, полный список — в .env.example.
// Секреты приходят только файлами (*_FILE): env видно через docker inspect и /proc/<pid>/environ.
// Всё корпоративное (домен Jira, логины, пароли, VPN) лежит в зашифрованном хранилище, а не здесь
package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/devWaylander/auto-time-report/internal/secret"
)

type Config struct {
	TelegramToken     secret.String
	TelegramOwnerID   int64
	TelegramChannelID int64

	DefaultIssue string
	StartTime    string // ЧЧ:ММ, время начала каждого worklog'а

	// Зашифрованное хранилище с корпоративными доступами, см. cmd/vault
	CredsFile string
	// Каталог в tmpfs, общий с VPN-sidecar'ом: временный конфиг и management-сокет
	VPNRunDir      string
	ConnectTimeout time.Duration

	Timezone    *time.Location
	CalendarURL string
	DatabaseURL string
}

var issueKeyRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]*-[0-9]+$`)

// Load читает и проверяет конфиг; ошибки по всем переменным возвращаются разом
func Load() (Config, error) {
	var (
		c    Config
		errs []error
		err  error
	)
	req := func(name string) string {
		v := strings.TrimSpace(os.Getenv(name))
		if v == "" {
			errs = append(errs, fmt.Errorf("%s is required", name))
		}
		return v
	}
	opt := func(name, def string) string {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v
		}
		return def
	}
	check := func(name string, err error) {
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}

	if path := req("TELEGRAM_BOT_TOKEN_FILE"); path != "" {
		token, err := readSecretFile(path)
		check("TELEGRAM_BOT_TOKEN_FILE", err)
		c.TelegramToken = token
	}
	c.TelegramOwnerID, err = strconv.ParseInt(req("TELEGRAM_OWNER_ID"), 10, 64)
	check("TELEGRAM_OWNER_ID", err)
	c.TelegramChannelID, err = strconv.ParseInt(req("TELEGRAM_CHANNEL_ID"), 10, 64)
	check("TELEGRAM_CHANNEL_ID", err)

	c.DefaultIssue = opt("JIRA_DEFAULT_ISSUE_KEY", "")
	if c.DefaultIssue != "" && !issueKeyRe.MatchString(c.DefaultIssue) {
		errs = append(errs, fmt.Errorf("JIRA_DEFAULT_ISSUE_KEY: %q is not an issue key", c.DefaultIssue))
	}
	c.StartTime = opt("JIRA_WORKLOG_START_TIME", "10:00")
	_, err = time.Parse("15:04", c.StartTime)
	check("JIRA_WORKLOG_START_TIME", err)

	c.CredsFile = req("CREDS_FILE")
	c.VPNRunDir = opt("VPN_RUN_DIR", "/run/vpn")
	c.ConnectTimeout, err = time.ParseDuration(opt("OPENVPN_CONNECT_TIMEOUT", "90s"))
	check("OPENVPN_CONNECT_TIMEOUT", err)

	c.Timezone, err = time.LoadLocation(opt("TIMEZONE", "Europe/Moscow"))
	check("TIMEZONE", err)
	c.CalendarURL = strings.TrimRight(opt("PRODUCTION_CALENDAR_URL", "https://isdayoff.ru"), "/")
	c.DatabaseURL = req("DATABASE_URL")

	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	return c, nil
}

func readSecretFile(path string) (secret.String, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return "", errors.New("file is empty")
	}
	return secret.String(v), nil
}
