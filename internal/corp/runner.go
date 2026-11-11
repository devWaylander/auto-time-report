// Package corp - всё, что касается корпоративной сети: зашифрованные доступы, VPN и запись в Jira.
// Бот зависит от него только через интерфейс, поэтому пакет можно вынести на отдельную машину
package corp

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"time"

	"github.com/devWaylander/auto-time-report/internal/jira"
)

// WorklogJob - один worklog к записи
type WorklogJob struct {
	IssueKey string
	Date     time.Time
	Duration time.Duration
	Comment  string
}

// Job - что сделать за одну сессию
type Job struct {
	Worklogs []WorklogJob
	// Периоды, в которые бот уже писал раньше: при отправке на проверку они тоже уходят
	KnownPeriods    []int64
	RequestApproval bool
}

type WorklogStatus int

const (
	StatusSkipped WorklogStatus = iota
	StatusCreated
	StatusAlreadyInJira
	StatusFailed
)

type WorklogResult struct {
	Job       WorklogJob
	Status    WorklogStatus
	WorklogID int64
	PeriodID  int64
	// InJira - сколько уже стояло в Jira за этот день, для StatusAlreadyInJira
	InJira time.Duration
	Err    error
}

type Outcome struct {
	Worklogs    []WorklogResult
	Approved    []int64
	CalendarURL string
}

// Hooks - обратная связь во время отправки
type Hooks struct {
	// Progress - короткие сообщения о ходе отправки для пользователя
	Progress func(msg string)
	// Created вызывается сразу после каждого созданного worklog'а, чтобы бот успел его сохранить;
	// ошибка останавливает отправку, иначе в Jira окажутся worklog'и, о которых бот не знает
	Created func(WorklogResult) error
	// Found вызывается для дня, который уже есть в Jira и создан не ботом: бот запоминает его, чтобы
	// не поднимать VPN ради него снова. Может быть nil
	Found func(WorklogResult)
}

type Runner struct {
	credsFile      string
	tunnel         *Tunnel
	startTime      string
	sessionTimeout time.Duration
}

func NewRunner(credsFile string, tunnel *Tunnel, startTime string) *Runner {
	return &Runner{
		credsFile: credsFile,
		tunnel:    tunnel,
		startTime: startTime,
		// Жёсткий потолок на всю сессию вместе с VPN: туннель не должен висеть дольше нужного
		sessionTimeout: 5 * time.Minute,
	}
}

// Run расшифровывает доступы, поднимает VPN, пишет worklog'и и при необходимости отправляет периоды
// на проверку. passphrase затирается. Первая же ошибка останавливает отправку, ретраев нет
func (r *Runner) Run(ctx context.Context, passphrase []byte, job Job, hooks Hooks) (Outcome, error) {
	creds, err := Open(r.credsFile, passphrase)
	clear(passphrase)
	if err != nil {
		return Outcome{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, r.sessionTimeout)
	defer cancel()

	if creds.VPN != nil {
		hooks.Progress("🔐 Подключение VPN. Направлен ОДИН пуш Multifactor\n" +
			"Второй пуш или пуш без нажатия кнопки необходимо отклонить")
		sess, err := r.tunnel.Up(ctx, *creds.VPN)
		if err != nil {
			return Outcome{}, err
		}
		defer sess.Close()
	}

	// Хранилище уже проверено в Open, ошибки разбора здесь невозможны
	base, _ := url.Parse(creds.Jira.BaseURL)
	extra, _ := url.ParseQuery(creds.Jira.ExtraFields)
	rootCAs, _ := creds.Jira.RootCAs()
	client := jira.New(jira.Options{
		RootCAs:     rootCAs,
		BaseURL:     base,
		IP:          creds.Jira.IP,
		Username:    creds.Jira.Username,
		Password:    creds.Jira.Password,
		StartTime:   r.startTime,
		ExtraFields: extra,
	})
	out := Outcome{CalendarURL: client.CalendarURL()}

	hooks.Progress("🔑 Проверка доступа к Jira…")
	if err := client.CheckAuth(ctx); err != nil {
		return out, err
	}
	var issues []string
	for _, w := range job.Worklogs {
		if !slices.Contains(issues, w.IssueKey) {
			issues = append(issues, w.IssueKey)
		}
	}
	for _, key := range issues {
		if err := client.CheckIssue(ctx, key); err != nil {
			return out, err
		}
	}

	out.Worklogs = make([]WorklogResult, len(job.Worklogs))
	for i, w := range job.Worklogs {
		out.Worklogs[i] = WorklogResult{Job: w, Status: StatusSkipped}
	}
	for i, w := range job.Worklogs {
		res := &out.Worklogs[i]
		hooks.Progress(fmt.Sprintf("✍️ Запись %s за %s (%d из %d)…", w.IssueKey, w.Date.Format("02.01"), i+1, len(job.Worklogs)))

		logged, err := client.LoggedOn(ctx, w.IssueKey, w.Date)
		if err != nil {
			res.Status, res.Err = StatusFailed, err
			return out, err
		}
		if logged > 0 {
			res.Status, res.InJira = StatusAlreadyInJira, logged
			if hooks.Found != nil {
				hooks.Found(*res)
			}
			continue
		}

		created, err := client.CreateWorklog(ctx, jira.WorklogInput{
			IssueKey: w.IssueKey, Date: w.Date, Duration: w.Duration, Comment: w.Comment,
		})
		if err != nil {
			res.Status, res.Err = StatusFailed, err
			return out, err
		}
		res.Status, res.WorklogID, res.PeriodID = StatusCreated, created.WorklogID, created.PeriodID
		if err := hooks.Created(*res); err != nil {
			return out, fmt.Errorf("save worklog %d: %w", created.WorklogID, err)
		}
	}

	if !job.RequestApproval {
		return out, nil
	}
	periods := slices.Clone(job.KnownPeriods)
	for _, res := range out.Worklogs {
		if res.Status == StatusCreated && !slices.Contains(periods, res.PeriodID) {
			periods = append(periods, res.PeriodID)
		}
	}
	if len(periods) == 0 {
		return out, errors.New("нет известных периодов для отправки на проверку")
	}
	slices.Sort(periods)
	for _, p := range periods {
		hooks.Progress(fmt.Sprintf("📨 Отправка периода %d на проверку…", p))
		if err := client.RequestApproval(ctx, p); err != nil {
			return out, err
		}
		out.Approved = append(out.Approved, p)
	}
	return out, nil
}
