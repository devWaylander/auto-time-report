package corp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/devWaylander/auto-time-report/internal/secret"
)

// ErrVPNAuth - VPN-сервер отклонил логин и пароль или пуш MFA
var ErrVPNAuth = errors.New("vpn: authentication failed")

const (
	tunnelConfigName = "client.ovpn"
	tunnelSocketName = "mgmt.sock"
)

// Tunnel управляет OpenVPN в sidecar-контейнере через общий каталог в tmpfs: бот кладёт туда конфиг,
// sidecar запускает openvpn, дальше всё общение - через management-сокет. Пароль в файлы не пишется
type Tunnel struct {
	runDir  string
	timeout time.Duration
}

func NewTunnel(runDir string, timeout time.Duration) *Tunnel {
	return &Tunnel{runDir: runDir, timeout: timeout}
}

// Session - поднятый туннель; Close обязателен
type Session struct {
	conn   net.Conn
	lines  <-chan string
	runDir string
}

// Up поднимает туннель и ждёт подключения, включая подтверждение пуша MFA
func (t *Tunnel) Up(ctx context.Context, creds VPNCredentials) (*Session, error) {
	routes, err := parseRoutes(creds.Routes)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()

	cfgPath := filepath.Join(t.runDir, tunnelConfigName)
	sockPath := filepath.Join(t.runDir, tunnelSocketName)
	// Бот мог упасть посреди прошлой сессии: такой openvpn ещё держит туннель, и sidecar занят им
	if err := stopOrphan(ctx, sockPath); err != nil {
		return nil, err
	}
	_ = os.Remove(cfgPath)

	// Пишем во временный файл и переименовываем: sidecar не должен увидеть недописанный конфиг
	// без наших директив. Группа читает: sidecar работает от root без CAP_DAC_OVERRIDE и состоит в группе бота
	tmpPath := cfgPath + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(buildConfig(creds.Config, sockPath, routes)), 0o640); err != nil {
		return nil, fmt.Errorf("vpn: write config: %w", err)
	}
	if err := os.Rename(tmpPath, cfgPath); err != nil {
		_ = os.Remove(tmpPath)
		return nil, fmt.Errorf("vpn: write config: %w", err)
	}
	// Конфиг нужен openvpn только на старте: после появления сокета он уже прочитан
	defer func() { _ = os.Remove(cfgPath) }()

	conn, err := dialWhenReady(ctx, sockPath)
	if err != nil {
		return nil, err
	}
	s := &Session{conn: conn, lines: readLines(conn), runDir: t.runDir}
	_ = os.Remove(cfgPath)

	if err := s.connect(ctx, creds); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// buildConfig дописывает к конфигу пользователя наши директивы; более поздние перекрывают ранние
func buildConfig(userConfig, sockPath string, routes []*net.IPNet) string {
	var b strings.Builder
	b.WriteString(userConfig)
	b.WriteString("\n\n# --- auto-time-report ---\n")
	fmt.Fprintf(&b, "management %s unix\n", sockPath)
	b.WriteString("management-hold\n")
	b.WriteString("management-query-passwords\n")
	// Логин и пароль спрашиваются через сокет, не из файла
	b.WriteString("auth-user-pass\n")
	b.WriteString("auth-nocache\n")
	// Ошибка авторизации завершает openvpn, без повторных пушей
	b.WriteString("auth-retry none\n")
	// Маршруты от сервера игнорируем, через туннель идёт только перечисленное
	b.WriteString("route-nopull\n")
	for _, r := range routes {
		fmt.Fprintf(&b, "route %s %s\n", r.IP, net.IP(r.Mask))
	}
	b.WriteString("verb 3\n")
	return b.String()
}

// stopOrphan гасит openvpn, оставшийся от прошлой сессии, и ждёт, пока sidecar уберёт его сокет
func stopOrphan(ctx context.Context, sockPath string) error {
	if _, err := os.Stat(sockPath); err != nil {
		return nil
	}
	conn, err := net.DialTimeout("unix", sockPath, time.Second)
	if err != nil {
		// Сокет есть, но никто не слушает: мусор после падения sidecar'а
		_ = os.Remove(sockPath)
		return nil
	}
	_, _ = conn.Write([]byte("signal SIGTERM\n"))
	_ = conn.Close()

	for {
		if _, err := os.Stat(sockPath); err != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("vpn: previous openvpn session did not stop: %w", ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func dialWhenReady(ctx context.Context, sockPath string) (net.Conn, error) {
	var d net.Dialer
	for {
		conn, err := d.DialContext(ctx, "unix", sockPath)
		if err == nil {
			return conn, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("vpn: sidecar did not start openvpn: %w", ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func readLines(conn net.Conn) <-chan string {
	ch := make(chan string)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(conn)
		for sc.Scan() {
			ch <- strings.TrimRight(sc.Text(), "\r")
		}
	}()
	return ch
}

func (s *Session) connect(ctx context.Context, creds VPNCredentials) error {
	if err := s.send("state on"); err != nil {
		return err
	}
	if err := s.send("hold release"); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("vpn: not connected in time (push not approved?): %w", ctx.Err())
		case line, ok := <-s.lines:
			if !ok {
				return errors.New("vpn: openvpn closed the management connection")
			}
			switch {
			case strings.HasPrefix(line, ">PASSWORD:Need 'Auth'"):
				if strings.Contains(line, "SC:") || strings.Contains(line, "CRV1:") {
					return errors.New("vpn: server asks for a challenge response, only push MFA is supported")
				}
				if err := s.sendCredentials(creds.Username, creds.Password); err != nil {
					return err
				}
			case strings.HasPrefix(line, ">PASSWORD:Verification Failed"):
				return ErrVPNAuth
			case strings.HasPrefix(line, ">FATAL:"):
				// Текст не показываем: в нём бывает адрес корпоративного сервера. Подробности - с VPN_DEBUG=1
				return errors.New("vpn: openvpn stopped with a fatal error, details with VPN_DEBUG=1")
			case strings.HasPrefix(line, ">STATE:") && strings.Contains(line, ",CONNECTED,"):
				return nil
			case strings.HasPrefix(line, ">STATE:") && strings.Contains(line, ",EXITING,"):
				return errors.New("vpn: openvpn exited before connecting")
			}
		}
	}
}

func (s *Session) sendCredentials(username string, password secret.String) error {
	// Перевод строки завершает команду протокола: через него можно подсунуть свою команду
	if strings.ContainsAny(username, "\r\n") || strings.ContainsAny(password.Reveal(), "\r\n") {
		return errors.New("vpn: username and password must not contain line breaks")
	}
	if err := s.send(`username "Auth" ` + quote(username)); err != nil {
		return err
	}
	line := []byte(`password "Auth" ` + quote(password.Reveal()) + "\n")
	defer clear(line)
	if _, err := s.conn.Write(line); err != nil {
		return fmt.Errorf("vpn: send password: %w", err)
	}
	return nil
}

func (s *Session) send(cmd string) error {
	if _, err := s.conn.Write([]byte(cmd + "\n")); err != nil {
		return fmt.Errorf("vpn: management: %w", err)
	}
	return nil
}

// Close гасит openvpn и ждёт, пока он закроет сокет
func (s *Session) Close() {
	_ = s.send("signal SIGTERM")
	timeout := time.After(10 * time.Second)
wait:
	for {
		select {
		case _, ok := <-s.lines:
			if !ok {
				break wait
			}
		case <-timeout:
			break wait
		}
	}
	_ = s.conn.Close()
	_ = os.Remove(filepath.Join(s.runDir, tunnelConfigName))
}

// quote экранирует значение для management-протокола OpenVPN
func quote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}
