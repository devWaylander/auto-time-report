// Точка входа бота: конфиг, база, напоминания и Telegram
package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

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

	// Если Telegram недоступен долго, выходим: Docker перезапустит бота, и он заново подключится к сети
	// VPN-sidecar'а. После перезапуска sidecar'а старое сетевое пространство бота остаётся без сети
	lost := make(chan error, 1)
	watchCtx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	go func() {
		if err := watchdog(watchCtx, b); err != nil {
			lost <- err
			stop()
		}
	}()

	slog.Info("bot started")
	b.Start(ctx)
	slog.Info("shutting down, waiting for the current submission")
	b.Wait()

	select {
	case err := <-lost:
		return err
	default:
		return nil
	}
}

const (
	watchdogInterval = time.Minute
	watchdogFailures = 5
)

// watchdog проверяет связь с Telegram и возвращает ошибку после watchdogFailures неудач подряд
func watchdog(ctx context.Context, b *bot.Bot) error {
	failures := 0
	ticker := time.NewTicker(watchdogInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		pingCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := b.Ping(pingCtx)
		cancel()
		switch {
		case err == nil:
			failures = 0
		case ctx.Err() != nil:
			return nil
		default:
			failures++
			slog.Warn("telegram is unreachable", "failures", failures, "error", err)
			if failures >= watchdogFailures {
				return fmt.Errorf("telegram unreachable for %d minutes: %w", failures, err)
			}
		}
	}
}
