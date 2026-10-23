package jira

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return New(Options{
		BaseURL: base, Username: "john.doe", Password: "secret-pass", StartTime: "10:00",
		ExtraFields: url.Values{"wa_1": {"4"}, "wa_2": {"OPEN"}, "issueKey": {"EVIL-1"}},
	})
}

func checkAuth(t *testing.T, r *http.Request) {
	t.Helper()
	user, pass, ok := r.BasicAuth()
	if !ok || user != "john.doe" || pass != "secret-pass" {
		t.Errorf("basic auth = %q %q %v", user, pass, ok)
	}
	if r.Header.Get("X-Atlassian-Token") != "no-check" {
		t.Error("no X-Atlassian-Token header")
	}
}

func TestCreateWorklog(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r)
		if r.Method != http.MethodPost || r.URL.Path != worklogFormPath {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("X-Requested-With") != "XMLHttpRequest" {
			t.Error("no X-Requested-With header")
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		want := map[string]string{
			"issueKey":              "ABC-1",
			"worklogAuthorUsername": "john.doe",
			"timeLogged":            "7h 30m",
			"startDate":             "2026-12-31 10:00",
			"startDateJS":           "31.12.26 10:00",
			"endDate":               "2027-01-01",
			"endDateJS":             "01.01.27",
			"period":                "none",
			"inline":                "true",
			"decorator":             "dialog",
			"comment":               "XYZ-1: работа - 7 часов 30 минут",
			"wa_1":                  "4",
			"wa_2":                  "OPEN",
		}
		for k, v := range want {
			if got := r.PostForm.Get(k); got != v {
				t.Errorf("form %s = %q, want %q", k, got, v)
			}
		}
		w.Header().Set("X-Atlassian-Dialog-Control", "DONE")
		w.Header().Set("X-Worklogpro_workloglist", "1234567")
		w.Header().Set("X-Worklogpro_periodid", "228")
		_, _ = w.Write([]byte("<html>page with private stuff</html>"))
	})

	got, err := c.CreateWorklog(context.Background(), WorklogInput{
		IssueKey: "ABC-1", Date: time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC),
		Duration: 7*time.Hour + 30*time.Minute, Comment: "XYZ-1: работа - 7 часов 30 минут",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.WorklogID != 1234567 || got.PeriodID != 228 {
		t.Fatalf("created = %+v", got)
	}
}

func TestCreateWorklogRejected(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>form with an error and private stuff</html>"))
	})
	_, err := c.CreateWorklog(context.Background(), WorklogInput{
		IssueKey: "ABC-1", Date: time.Now(), Duration: time.Hour, Comment: "x",
	})
	if err == nil {
		t.Fatal("expected error without dialog control DONE")
	}
	if strings.Contains(err.Error(), "private") {
		t.Fatalf("response body leaked into error: %v", err)
	}
}

func TestCreateWorklogUnknownResult(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Atlassian-Dialog-Control", "DONE")
	})
	_, err := c.CreateWorklog(context.Background(), WorklogInput{
		IssueKey: "ABC-1", Date: time.Now(), Duration: time.Hour, Comment: "x",
	})
	if !errors.Is(err, ErrUnknownResult) {
		t.Fatalf("error = %v, want ErrUnknownResult", err)
	}
}

func TestInvalidIssueKeyNeverReachesServer(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("request reached server: %s", r.URL)
	})
	ctx := context.Background()
	for _, key := range []string{"../../admin", "ABC-1?x=y", "abc-1", ""} {
		if err := c.CheckIssue(ctx, key); err == nil {
			t.Errorf("CheckIssue(%q) accepted", key)
		}
		if _, err := c.HasWorklogOn(ctx, key, time.Now()); err == nil {
			t.Errorf("HasWorklogOn(%q) accepted", key)
		}
		if _, err := c.CreateWorklog(ctx, WorklogInput{IssueKey: key, Date: time.Now(), Duration: time.Hour}); err == nil {
			t.Errorf("CreateWorklog(%q) accepted", key)
		}
	}
}

func TestAuthErrors(t *testing.T) {
	tests := []struct {
		name string
		h    http.HandlerFunc
	}{
		{"401", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) }},
		{"seraph failed", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Seraph-LoginReason", "AUTHENTICATED_FAILED")
			w.WriteHeader(http.StatusForbidden)
		}},
		{"captcha", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Authentication-Denied-Reason", "CAPTCHA_CHALLENGE; login-url=/login.jsp")
			w.WriteHeader(http.StatusForbidden)
		}},
		{"redirect to login", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/login.jsp?os_destination=x", http.StatusFound)
		}},
		{"wrong user", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-AUSERNAME", "anonymous")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestClient(t, tt.h)
			if err := c.CheckAuth(context.Background()); !errors.Is(err, ErrAuth) {
				t.Fatalf("error = %v, want ErrAuth", err)
			}
		})
	}
}

func TestCheckAuthOK(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r)
		w.Header().Set("X-Seraph-LoginReason", "OK")
		w.Header().Set("X-AUSERNAME", "john.doe")
	})
	if err := c.CheckAuth(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCheckIssue(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rest/api/2/issue/ABC-404" {
			w.WriteHeader(http.StatusNotFound)
		}
	})
	if err := c.CheckIssue(context.Background(), "ABC-1"); err != nil {
		t.Fatal(err)
	}
	if err := c.CheckIssue(context.Background(), "ABC-404"); !errors.Is(err, ErrIssueNotFound) {
		t.Fatalf("error = %v, want ErrIssueNotFound", err)
	}
}

func TestHasWorklogOn(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"worklogs":[
			{"author":{"name":"someone.else"},"started":"2026-10-19T10:00:00.000+0700"},
			{"author":{"name":"John.Doe"},"started":"2026-10-20T10:00:00.000+0700"}
		]}`))
	})
	ctx := context.Background()
	for d, want := range map[string]bool{"2026-10-19": false, "2026-10-20": true, "2026-10-21": false} {
		date, _ := time.Parse("2006-01-02", d)
		got, err := c.HasWorklogOn(ctx, "ABC-1", date)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("HasWorklogOn(%s) = %v, want %v", d, got, want)
		}
	}
}

func TestRequestApproval(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r)
		if r.Method != http.MethodPut || r.URL.Path != "/rest/com.deniz.jira.worklog/latest/approval/period/228/request" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	if err := c.RequestApproval(context.Background(), 228); err != nil {
		t.Fatal(err)
	}
}

func TestPinnedIP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "jira.example.com:"+strings.Split(r.Host, ":")[1] {
			t.Errorf("Host header = %q, want jira.example.com", r.Host)
		}
		w.Header().Set("X-AUSERNAME", "john.doe")
	}))
	defer srv.Close()
	srvURL, _ := url.Parse(srv.URL)

	base, _ := url.Parse("http://jira.example.com:" + srvURL.Port())
	c := New(Options{BaseURL: base, IP: srvURL.Hostname(), Username: "john.doe", Password: "x"})
	if err := c.CheckAuth(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestFormatDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		8 * time.Hour:                "8h",
		7*time.Hour + 30*time.Minute: "7h 30m",
		45 * time.Minute:             "45m",
	} {
		if got := FormatDuration(d); got != want {
			t.Errorf("FormatDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestNetworkErrorHidesAddresses(t *testing.T) {
	ctx := context.Background()

	base, _ := url.Parse("https://jira.secret-corp.test:1")
	c := New(Options{BaseURL: base, IP: "127.0.0.1", Username: "john.doe", Password: "x"})
	err := c.CheckAuth(ctx)
	if err == nil {
		t.Fatal("expected connection error")
	}
	for _, leak := range []string{"secret-corp", "127.0.0.1"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error leaks %q: %v", leak, err)
		}
	}

	base, _ = url.Parse("https://jira.secret-corp.invalid")
	c = New(Options{BaseURL: base, Username: "john.doe", Password: "x"})
	err = c.CheckAuth(ctx)
	if err == nil || strings.Contains(err.Error(), "secret-corp") || !strings.Contains(err.Error(), "dns") {
		t.Errorf("dns error = %v", err)
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := c.CheckAuth(canceled); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled error = %v, want context.Canceled", err)
	}
}

func TestContextPathAndTrailingSlash(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/jira/rest/api/2/myself" {
			t.Errorf("path = %s, want /jira/rest/api/2/myself", r.URL.Path)
		}
		w.Header().Set("X-AUSERNAME", "john.doe")
	}))
	defer srv.Close()

	base, _ := url.Parse(srv.URL + "/jira/")
	c := New(Options{BaseURL: base, Username: "john.doe", Password: "x"})
	if err := c.CheckAuth(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := c.CalendarURL(); got != srv.URL+"/jira"+calendarPath {
		t.Fatalf("calendar url = %s", got)
	}
}
