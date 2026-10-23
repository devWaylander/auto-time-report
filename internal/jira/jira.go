// Package jira ходит в Jira Server и плагин WorklogPRO: создаёт worklog'и и отправляет периоды на проверку.
// Разбор API - в docs/jira-api.md
package jira

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/devWaylander/auto-time-report/internal/secret"
)

var (
	// ErrAuth - Jira не приняла логин и пароль. Повторять нельзя: после нескольких попыток включится CAPTCHA
	ErrAuth = errors.New("jira: authentication failed")
	// ErrIssueNotFound - задачи нет или к ней нет доступа
	ErrIssueNotFound = errors.New("jira: issue not found")
	// ErrUnknownResult - запрос ушёл, но по ответу нельзя понять, создан ли worklog
	ErrUnknownResult = errors.New("jira: unknown worklog result")
)

var issueKeyRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]*-[0-9]+$`)

const (
	maxBody         = 10 << 20
	worklogFormPath = "/secure/WPCreateWorklogOnIssueWithWorkLogAction.jspa"
	calendarPath    = "/secure/WorklogPROWorklogCalendar!default.jspa"
)

// Options - доступ к Jira; всё, кроме StartTime, приходит из зашифрованного хранилища
type Options struct {
	BaseURL *url.URL
	// Необязательный IP Jira: если корпоративный DNS недоступен через туннель, ходим по нему,
	// сохраняя имя хоста для TLS
	IP          string
	Username    string
	Password    secret.String
	StartTime   string // ЧЧ:ММ
	ExtraFields url.Values
}

type Client struct {
	base      *url.URL
	username  string
	password  secret.String
	startTime string
	extra     url.Values
	http      *http.Client
}

func New(cfg Options) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Никаких прокси из окружения: пароль должен идти только напрямую через туннель
	transport.Proxy = nil
	if cfg.IP != "" {
		port := cfg.BaseURL.Port()
		if port == "" {
			port = "443"
		}
		target := net.JoinHostPort(cfg.BaseURL.Hostname(), port)
		pinned := net.JoinHostPort(cfg.IP, port)
		dialer := &net.Dialer{Timeout: 10 * time.Second}
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if addr == target {
				addr = pinned
			}
			return dialer.DialContext(ctx, network, addr)
		}
	}

	return &Client{
		base:      cfg.BaseURL,
		username:  cfg.Username,
		password:  cfg.Password,
		startTime: cfg.StartTime,
		extra:     cfg.ExtraFields,
		http: &http.Client{
			Timeout:   30 * time.Second,
			Transport: transport,
			// Редирект от Jira - это почти всегда страница логина; Basic auth за ним не отправляем
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// CalendarURL - ссылка на календарь WorklogPRO для итогового сообщения
func (c *Client) CalendarURL() string {
	return c.base.String() + calendarPath
}

// CheckAuth проверяет логин и пароль одним запросом
func (c *Client) CheckAuth(ctx context.Context) error {
	resp, err := c.do(ctx, http.MethodGet, "/rest/api/2/myself", nil, nil)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jira: check auth: status %d", resp.StatusCode)
	}
	if !strings.EqualFold(resp.Header.Get("X-Ausername"), c.username) {
		return fmt.Errorf("%w: server did not recognize the user", ErrAuth)
	}
	return nil
}

// CheckIssue проверяет, что задача существует и доступна
func (c *Client) CheckIssue(ctx context.Context, key string) error {
	if !issueKeyRe.MatchString(key) {
		return fmt.Errorf("jira: invalid issue key %q", key)
	}
	resp, err := c.do(ctx, http.MethodGet, "/rest/api/2/issue/"+key+"?fields=summary", nil, nil)
	if err != nil {
		return err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrIssueNotFound, key)
	default:
		return fmt.Errorf("jira: check issue %s: status %d", key, resp.StatusCode)
	}
}

// HasWorklogOn сообщает, есть ли в задаче worklog текущего пользователя на эту дату (поставленный не ботом)
func (c *Client) HasWorklogOn(ctx context.Context, key string, date time.Time) (bool, error) {
	if !issueKeyRe.MatchString(key) {
		return false, fmt.Errorf("jira: invalid issue key %q", key)
	}
	var body struct {
		Worklogs []struct {
			Author struct {
				Name string `json:"name"`
			} `json:"author"`
			Started string `json:"started"`
		} `json:"worklogs"`
	}
	resp, err := c.do(ctx, http.MethodGet, "/rest/api/2/issue/"+key+"/worklog", nil, &body)
	if err != nil {
		return false, err
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("jira: list worklogs %s: status %d", key, resp.StatusCode)
	}

	day := date.Format("2006-01-02")
	for _, w := range body.Worklogs {
		if strings.EqualFold(w.Author.Name, c.username) && strings.HasPrefix(w.Started, day) {
			return true, nil
		}
	}
	return false, nil
}

// WorklogInput - что записать
type WorklogInput struct {
	IssueKey string
	Date     time.Time
	Duration time.Duration
	Comment  string
}

// Created - идентификаторы из ответа WorklogPRO
type Created struct {
	WorklogID int64
	PeriodID  int64
}

// CreateWorklog отправляет форму WorklogPRO так же, как браузер
func (c *Client) CreateWorklog(ctx context.Context, in WorklogInput) (Created, error) {
	if !issueKeyRe.MatchString(in.IssueKey) {
		return Created{}, fmt.Errorf("jira: invalid issue key %q", in.IssueKey)
	}
	if in.Duration < time.Minute {
		return Created{}, fmt.Errorf("jira: duration %v is too short", in.Duration)
	}

	form := url.Values{}
	for k, v := range c.extra {
		form[k] = v
	}
	next := in.Date.AddDate(0, 0, 1)
	for k, v := range map[string]string{
		"inline":                           "true",
		"decorator":                        "dialog",
		"jql":                              "",
		"navFilter":                        "",
		"issueKey":                         in.IssueKey,
		"filterByProjectKey":               "",
		"worklogAuthorUsername":            c.username,
		"period":                           "none",
		"timeLogged":                       FormatDuration(in.Duration),
		"startDateJS":                      in.Date.Format("02.01.06") + " " + c.startTime,
		"startDate":                        in.Date.Format("2006-01-02") + " " + c.startTime,
		"endDateJS":                        next.Format("02.01.06"),
		"endDate":                          next.Format("2006-01-02"),
		"adjustEstimate":                   "auto",
		"remainingEstimateOptionsExpanded": "false",
		"comment":                          in.Comment,
		"commentLevel":                     "",
	} {
		form.Set(k, v)
	}

	headers := http.Header{
		"Content-Type":     {"application/x-www-form-urlencoded; charset=UTF-8"},
		"X-Requested-With": {"XMLHttpRequest"},
	}
	resp, err := c.do(ctx, http.MethodPost, worklogFormPath, strings.NewReader(form.Encode()), nil, headers)
	if err != nil {
		return Created{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return Created{}, fmt.Errorf("jira: create worklog %s %s: status %d",
			in.IssueKey, in.Date.Format("02.01"), resp.StatusCode)
	}
	if control := resp.Header.Get("X-Atlassian-Dialog-Control"); control != "DONE" {
		return Created{}, fmt.Errorf("jira: worklog %s %s rejected by the form (dialog control %q)",
			in.IssueKey, in.Date.Format("02.01"), control)
	}

	worklogID, err1 := firstID(resp.Header.Get("X-Worklogpro_workloglist"))
	periodID, err2 := firstID(resp.Header.Get("X-Worklogpro_periodid"))
	if err := errors.Join(err1, err2); err != nil {
		return Created{}, fmt.Errorf("%w: %s %s: %v", ErrUnknownResult, in.IssueKey, in.Date.Format("02.01"), err)
	}
	return Created{WorklogID: worklogID, PeriodID: periodID}, nil
}

// RequestApproval отправляет период таймшита на проверку
func (c *Client) RequestApproval(ctx context.Context, periodID int64) error {
	path := "/rest/com.deniz.jira.worklog/latest/approval/period/" + strconv.FormatInt(periodID, 10) + "/request?explanation="
	resp, err := c.do(ctx, http.MethodPut, path, nil, nil)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("jira: request approval for period %d: status %d", periodID, resp.StatusCode)
	}
	return nil
}

// do выполняет запрос с Basic auth. Тело ответа декодируется в out (если задан) и всегда закрывается;
// в ошибки и логи оно не попадает
func (c *Client) do(ctx context.Context, method, path string, body io.Reader, out any, headers ...http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base.String()+path, body)
	if err != nil {
		return nil, fmt.Errorf("jira: build request: %w", err)
	}
	req.SetBasicAuth(c.username, c.password.Reveal())
	req.Header.Set("Accept", "application/json, text/html, */*")
	req.Header.Set("User-Agent", "auto-time-report")
	// Отключает XSRF-проверку Jira для скриптов вместо atl_token из браузерной сессии
	req.Header.Set("X-Atlassian-Token", "no-check")
	for _, h := range headers {
		for k, v := range h {
			req.Header[k] = v
		}
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jira: %s %s: %w", method, req.URL.Path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := authError(resp); err != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
		return nil, err
	}
	if out != nil && resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(out); err != nil {
			return nil, fmt.Errorf("jira: decode %s: %w", req.URL.Path, err)
		}
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
	return resp, nil
}

func authError(resp *http.Response) error {
	reason := resp.Header.Get("X-Seraph-Loginreason")
	denied := resp.Header.Get("X-Authentication-Denied-Reason")
	switch {
	case denied != "":
		return fmt.Errorf("%w: %s", ErrAuth, denied)
	case strings.Contains(reason, "FAILED") || strings.Contains(reason, "DENIED"):
		return fmt.Errorf("%w: %s", ErrAuth, reason)
	case resp.StatusCode == http.StatusUnauthorized:
		return fmt.Errorf("%w: status 401", ErrAuth)
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return fmt.Errorf("%w: redirected to %s", ErrAuth, redirectPath(resp))
	}
	return nil
}

func redirectPath(resp *http.Response) string {
	loc, err := resp.Location()
	if err != nil {
		return "unknown location"
	}
	return loc.Path
}

var idRe = regexp.MustCompile(`[0-9]+`)

func firstID(header string) (int64, error) {
	m := idRe.FindString(header)
	if m == "" {
		return 0, fmt.Errorf("no id in %q", header)
	}
	return strconv.ParseInt(m, 10, 64)
}

// FormatDuration - длительность в формате Jira: "8h", "7h 30m", "45m"
func FormatDuration(d time.Duration) string {
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	switch {
	case h > 0 && m > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	case h > 0:
		return fmt.Sprintf("%dh", h)
	default:
		return fmt.Sprintf("%dm", m)
	}
}
