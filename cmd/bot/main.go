// Точка входа бота: конфиг, база, напоминания и Telegram
package main

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/joho/godotenv"

	"github.com/devWaylander/auto-time-report/internal/bot"
	"github.com/devWaylander/auto-time-report/internal/calendar"
	"github.com/devWaylander/auto-time-report/internal/config"
	"github.com/devWaylander/auto-time-report/internal/corp"
	"github.com/devWaylander/auto-time-report/internal/notify"
	"github.com/devWaylander/auto-time-report/internal/store"
)

func main() {
	if err := run(); err != nil {
		slog.Error("bot stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	// .env нужен для локального запуска; в Docker переменные приходят из compose,
	// а уже заданные переменные окружения godotenv не перезаписывает
	if err := godotenv.Load(); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer func() {
		if err := st.Close(); err != nil {
			slog.Error("close store", "error", err)
		}
	}()

	user, err := st.EnsureUser(ctx, cfg.TelegramOwnerID)
	if err != nil {
		return err
	}
	if _, err := os.Stat(cfg.CredsFile); err != nil {
		// Без хранилища бот всё равно полезен: пост, превью, напоминания. Отправка скажет об ошибке сама
		slog.Warn("creds file is not available, submitting to Jira will fail", "error", err)
	}

	cal := calendar.New(cfg.CalendarURL)
	runner := corp.NewRunner(cfg.CredsFile, corp.NewTunnel(cfg.VPNRunDir, cfg.ConnectTimeout), cfg.StartTime)
	b, err := bot.New(bot.Config{
		Token:        cfg.TelegramToken,
		OwnerID:      cfg.TelegramOwnerID,
		ChannelID:    cfg.TelegramChannelID,
		DefaultIssue: cfg.DefaultIssue,
		Location:     cfg.Timezone,
	}, st, cal, runner, user.ID)
	if err != nil {
		return err
	}

	go notify.New(st, cal, b, cfg.Timezone, user.ID, cfg.DefaultIssue).Run(ctx)

	slog.Info("bot started")
	b.Start(ctx)
	slog.Info("shutting down")
	return nil
}
