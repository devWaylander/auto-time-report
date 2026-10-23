package corp

import (
	"context"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

var phrase = []byte("correct horse battery staple")

// fakeJira изображает Jira с WorklogPRO по TLS; rejectForm - номер POST формы (с 1), на котором форма не примет worklog
func fakeJira(t *testing.T, rejectForm int32) (*httptest.Server, *atomic.Int32, *[]int64) {
	t.Helper()
	var forms atomic.Int32
	var approved []int64
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, pass, _ := r.BasicAuth(); user != "john.doe" || pass != "jira-pass" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/rest/api/2/myself":
			w.Header().Set("X-AUSERNAME", "john.doe")
		case r.URL.Path == "/rest/api/2/issue/ABC-1":
		case r.URL.Path == "/rest/api/2/issue/ABC-1/worklog":
			_, _ = w.Write([]byte(`{"worklogs":[{"author":{"name":"john.doe"},"started":"2026-10-20T10:00:00.000+0700"}]}`))
		case r.Method == http.MethodPost:
			n := forms.Add(1)
			if n == rejectForm {
				return
			}
			w.Header().Set("X-Atlassian-Dialog-Control", "DONE")
			w.Header().Set("X-Worklogpro_workloglist", strconv.Itoa(1000000+int(n)))
			w.Header().Set("X-Worklogpro_periodid", "228")
		case r.Method == http.MethodPut:
			id, _ := strconv.ParseInt(filepath.Base(filepath.Dir(r.URL.Path)), 10, 64)
			approved = append(approved, id)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &forms, &approved
}

func newRunner(t *testing.T, srv *httptest.Server) *Runner {
	t.Helper()
	creds := Credentials{Jira: JiraCredentials{
		BaseURL: srv.URL, Username: "john.doe", Password: "jira-pass",
		CACert: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})),
	}}
	blob, err := Seal(creds, phrase)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "creds.age")
	if err := os.WriteFile(path, blob, 0o400); err != nil {
		t.Fatal(err)
	}
	return NewRunner(path, NewTunnel(t.TempDir(), time.Second), "10:00")
}

func job(approve bool) Job {
	d := func(day int) time.Time { return time.Date(2026, 10, day, 0, 0, 0, 0, time.UTC) }
	return Job{
		Worklogs: []WorklogJob{
			{IssueKey: "ABC-1", Date: d(19), Duration: 8 * time.Hour, Comment: "a"},
			{IssueKey: "ABC-1", Date: d(20), Duration: 8 * time.Hour, Comment: "b"},
			{IssueKey: "ABC-1", Date: d(21), Duration: 8 * time.Hour, Comment: "c"},
		},
		KnownPeriods:    []int64{100},
		RequestApproval: approve,
	}
}

func TestRunnerSubmitsAndRequestsApproval(t *testing.T) {
	srv, forms, approved := fakeJira(t, 0)
	r := newRunner(t, srv)

	var saved []WorklogResult
	hooks := Hooks{
		Progress: func(string) {},
		Created:  func(res WorklogResult) error { saved = append(saved, res); return nil },
	}
	pass := append([]byte(nil), phrase...)
	out, err := r.Run(context.Background(), pass, job(true), hooks)
	if err != nil {
		t.Fatal(err)
	}
	for i, b := range pass {
		if b != 0 {
			t.Fatalf("passphrase byte %d is not wiped", i)
		}
	}

	want := []WorklogStatus{StatusCreated, StatusAlreadyInJira, StatusCreated}
	for i, res := range out.Worklogs {
		if res.Status != want[i] {
			t.Errorf("worklog %d status = %v, want %v", i, res.Status, want[i])
		}
	}
	if forms.Load() != 2 || len(saved) != 2 || saved[0].WorklogID != 1000001 || saved[0].PeriodID != 228 {
		t.Fatalf("forms %d, saved %+v", forms.Load(), saved)
	}
	if len(out.Approved) != 2 || out.Approved[0] != 100 || out.Approved[1] != 228 || len(*approved) != 2 {
		t.Fatalf("approved = %v (server saw %v), want [100 228]", out.Approved, *approved)
	}
	if out.CalendarURL != srv.URL+"/secure/WorklogPROWorklogCalendar!default.jspa" {
		t.Errorf("calendar url = %s", out.CalendarURL)
	}
}

func TestRunnerStopsOnFirstError(t *testing.T) {
	srv, _, approved := fakeJira(t, 2)
	r := newRunner(t, srv)

	hooks := Hooks{Progress: func(string) {}, Created: func(WorklogResult) error { return nil }}
	j := job(true)
	j.Worklogs[1].Date = j.Worklogs[1].Date.AddDate(0, 0, 3)
	out, err := r.Run(context.Background(), append([]byte(nil), phrase...), j, hooks)
	if err == nil {
		t.Fatal("expected error from rejected form")
	}
	want := []WorklogStatus{StatusCreated, StatusFailed, StatusSkipped}
	for i, res := range out.Worklogs {
		if res.Status != want[i] {
			t.Errorf("worklog %d status = %v, want %v", i, res.Status, want[i])
		}
	}
	if len(out.Approved) != 0 || len(*approved) != 0 {
		t.Fatalf("approval requested after a failure: %v", *approved)
	}
}

func TestRunnerStopsWhenSaveFails(t *testing.T) {
	srv, forms, _ := fakeJira(t, 0)
	r := newRunner(t, srv)

	hooks := Hooks{Progress: func(string) {}, Created: func(WorklogResult) error { return errors.New("disk full") }}
	if _, err := r.Run(context.Background(), append([]byte(nil), phrase...), job(false), hooks); err == nil {
		t.Fatal("expected error when the bot cannot save a worklog")
	}
	if forms.Load() != 1 {
		t.Fatalf("forms sent = %d, want 1", forms.Load())
	}
}

func TestRunnerBadPassphrase(t *testing.T) {
	srv, forms, _ := fakeJira(t, 0)
	r := newRunner(t, srv)
	hooks := Hooks{Progress: func(string) {}, Created: func(WorklogResult) error { return nil }}
	if _, err := r.Run(context.Background(), []byte("wrong passphrase, long enough"), job(true), hooks); !errors.Is(err, ErrBadPassphrase) {
		t.Fatalf("error = %v, want ErrBadPassphrase", err)
	}
	if forms.Load() != 0 {
		t.Fatal("requests sent with a wrong passphrase")
	}
}
