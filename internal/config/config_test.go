package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func setEnv(t *testing.T, tokenFile string) {
	t.Helper()
	for k, v := range map[string]string{
		"TELEGRAM_BOT_TOKEN_FILE": tokenFile,
		"TELEGRAM_OWNER_ID":       "123",
		"TELEGRAM_CHANNEL_ID":     "-1001",
		"JIRA_DEFAULT_ISSUE_KEY":  "ABC-1",
		"CREDS_FILE":              "/run/secrets/creds",
		"DATABASE_URL":            "sqlite:./data/bot.db",
		"TIMEZONE":                "Asia/Novosibirsk",
	} {
		t.Setenv(k, v)
	}
}

func TestLoad(t *testing.T) {
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("123:abc\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	setEnv(t, token)

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.TelegramToken.Reveal() != "123:abc" || c.TelegramOwnerID != 123 || c.TelegramChannelID != -1001 {
		t.Fatalf("telegram config = %+v", c)
	}
	if c.StartTime != "10:00" || c.VPNRunDir != "/run/vpn" || c.ConnectTimeout != 90*time.Second {
		t.Fatalf("defaults = %+v", c)
	}
	if c.Timezone.String() != "Asia/Novosibirsk" {
		t.Fatalf("timezone = %s", c.Timezone)
	}
	if strings.Contains(fmt.Sprintf("%v %+v %#v", c, c, c), "123:abc") {
		t.Fatal("token is printed with the config")
	}
}

func TestLoadErrors(t *testing.T) {
	setEnv(t, filepath.Join(t.TempDir(), "missing"))
	t.Setenv("TELEGRAM_OWNER_ID", "abc")
	t.Setenv("JIRA_DEFAULT_ISSUE_KEY", "not a key")
	t.Setenv("DATABASE_URL", "")

	_, err := Load()
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, name := range []string{"TELEGRAM_BOT_TOKEN_FILE", "TELEGRAM_OWNER_ID", "JIRA_DEFAULT_ISSUE_KEY", "DATABASE_URL"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error does not mention %s: %v", name, err)
		}
	}
}

func TestTelegramProxyURL(t *testing.T) {
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("123:abc"), 0o400); err != nil {
		t.Fatal(err)
	}
	setEnv(t, token)

	t.Setenv("TELEGRAM_PROXY_URL", "socks5://bot:s3cret@host.docker.internal:10808")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.TelegramProxyURL.Reveal() != "socks5://bot:s3cret@host.docker.internal:10808" {
		t.Fatalf("proxy = %q", c.TelegramProxyURL.Reveal())
	}
	if strings.Contains(fmt.Sprintf("%v %+v %#v", c, c, c), "s3cret") {
		t.Fatal("proxy password is printed with the config")
	}

	for _, bad := range []string{
		"ss://bot:s3cret@host:10808",
		"socks5://bot:s3cret@host",
		"host:10808",
		"http://bot:s3cret@:10808",
	} {
		t.Setenv("TELEGRAM_PROXY_URL", bad)
		_, err := Load()
		if err == nil || !strings.Contains(err.Error(), "TELEGRAM_PROXY_URL") {
			t.Errorf("%q: err = %v", bad, err)
		}
		if err != nil && strings.Contains(err.Error(), "s3cret") {
			t.Errorf("%q: password in error: %v", bad, err)
		}
	}
}
