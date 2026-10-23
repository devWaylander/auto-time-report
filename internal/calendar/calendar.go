// Package calendar знает рабочие, выходные и сокращённые дни по производственному календарю isdayoff.ru
package calendar

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// DayKind - тип дня по производственному календарю
type DayKind int

const (
	Working DayKind = iota
	Short
	Off
)

// Norm - сколько часов положено отработать в такой день
func (k DayKind) Norm() time.Duration {
	switch k {
	case Working:
		return 8 * time.Hour
	case Short:
		return 7 * time.Hour
	default:
		return 0
	}
}

// Calendar кэширует календарь по годам в памяти
type Calendar struct {
	baseURL string
	client  *http.Client

	mu    sync.Mutex
	years map[int]string
}

func New(baseURL string) *Calendar {
	return &Calendar{
		baseURL: baseURL,
		client:  &http.Client{Timeout: 15 * time.Second},
		years:   map[int]string{},
	}
}

// Day возвращает тип дня. Если API недоступно, возвращает обычную пятидневку и ошибку,
// чтобы вызывающий мог предупредить, но не остановиться
func (c *Calendar) Day(ctx context.Context, date time.Time) (DayKind, error) {
	days, err := c.year(ctx, date.Year())
	if err != nil {
		return fallback(date), err
	}
	idx := date.YearDay() - 1
	if idx >= len(days) {
		return fallback(date), fmt.Errorf("calendar for %d has %d days", date.Year(), len(days))
	}
	switch days[idx] {
	case '0', '4':
		return Working, nil
	case '2':
		return Short, nil
	default:
		return Off, nil
	}
}

func fallback(date time.Time) DayKind {
	if wd := date.Weekday(); wd == time.Saturday || wd == time.Sunday {
		return Off
	}
	return Working
}

func (c *Calendar) year(ctx context.Context, year int) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if days, ok := c.years[year]; ok {
		return days, nil
	}

	q := url.Values{"year": {strconv.Itoa(year)}, "pre": {"1"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/getdata?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch calendar %d: %w", year, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch calendar %d: status %d", year, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if err != nil {
		return "", fmt.Errorf("read calendar %d: %w", year, err)
	}

	days := string(body)
	want := time.Date(year, time.December, 31, 0, 0, 0, 0, time.UTC).YearDay()
	if len(days) != want {
		return "", fmt.Errorf("calendar %d: got %d days, want %d", year, len(days), want)
	}
	c.years[year] = days
	return days, nil
}
