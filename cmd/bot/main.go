// Точка входа бота. Пока только подключается к базе и ждёт сигнала остановки:
// Telegram, Jira и VPN появятся следующими шагами
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return errors.New("DATABASE_URL is not set")
	}

	st, err := store.Open(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer func() {
		if err := st.Close(); err != nil {
			slog.Error("close store", "error", err)
		}
	}()

	slog.Info("bot started")
	<-ctx.Done()
	slog.Info("shutting down")
	return nil
}
