package calendar

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDay(t *testing.T) {
	// 2026: 1 января выходной, 2 января сокращённый, остальное рабочие (для теста)
	days := "12" + strings.Repeat("0", 363)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Query().Get("year") != "2026" || r.URL.Query().Get("pre") != "1" {
			t.Errorf("unexpected query %s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(days))
	}))
	defer srv.Close()

	c := New(srv.URL)
	ctx := context.Background()
	for d, want := range map[time.Time]DayKind{
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC):   Off,
		time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC):   Short,
		time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC): Working,
	} {
		got, err := c.Day(ctx, d)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("Day(%s) = %v, want %v", d.Format("2006-01-02"), got, want)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("calendar fetched %d times, want 1 (cached)", calls.Load())
	}
}

func TestDayFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := New(srv.URL)
	sat := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	mon := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	if got, err := c.Day(context.Background(), sat); err == nil || got != Off {
		t.Fatalf("Day(sat) = %v, %v; want Off with error", got, err)
	}
	if got, err := c.Day(context.Background(), mon); err == nil || got != Working {
		t.Fatalf("Day(mon) = %v, %v; want Working with error", got, err)
	}
}

func TestNorm(t *testing.T) {
	if Working.Norm() != 8*time.Hour || Short.Norm() != 7*time.Hour || Off.Norm() != 0 {
		t.Fatal("unexpected norms")
	}
}
