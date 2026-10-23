// Package secret — строка, которая не печатается ни через fmt, ни в логах, ни при сериализации
package secret

import "log/slog"

type String string

// Reveal возвращает значение; вызывать только там, где оно реально уходит наружу (Jira, VPN, Telegram)
func (s String) Reveal() string { return string(s) }

func (s String) String() string               { return "***" }
func (s String) GoString() string             { return "***" }
func (s String) LogValue() slog.Value         { return slog.StringValue("***") }
func (s String) MarshalText() ([]byte, error) { return []byte("***"), nil }
