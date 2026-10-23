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
