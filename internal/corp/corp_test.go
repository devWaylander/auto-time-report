package corp

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testCreds() Credentials {
	return Credentials{
		Jira: JiraCredentials{
			BaseURL: "https://jira.example.com", Username: "john.doe", Password: "p@ss \"word\"",
			ExtraFields: "wa_1=1&wa_2=VALUE",
		},
		VPN: &VPNCredentials{
			Username: "john.doe", Password: `vpn\pa"ss`,
			Config: "client\nremote vpn.example.com 1194\n", Routes: []string{"10.1.2.3/32"},
		},
	}
}

func TestVaultRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "creds.age")
	phrase := []byte("correct horse battery staple")

	blob, err := Seal(testCreds(), phrase)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), "jira.example.com") {
		t.Fatal("blob contains plaintext")
	}
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := Open(path, []byte("correct horse battery staple"))
	if err != nil {
		t.Fatal(err)
	}
	want := testCreds()
	if got.Jira != want.Jira || got.VPN.Password != want.VPN.Password || got.VPN.Config != want.VPN.Config {
		t.Fatalf("got %+v", got)
	}

	if _, err := Open(path, []byte("wrong passphrase, long enough")); !errors.Is(err, ErrBadPassphrase) {
		t.Fatalf("wrong passphrase error = %v, want ErrBadPassphrase", err)
	}
}

func TestSealValidates(t *testing.T) {
	if _, err := Seal(testCreds(), []byte("short")); err == nil {
		t.Fatal("expected error for short passphrase")
	}

	c := testCreds()
	c.VPN.Routes = []string{"0.0.0.0/0"}
	if _, err := Seal(c, []byte("correct horse battery staple")); err == nil {
		t.Fatal("expected error for a route that captures all traffic")
	}

	c = testCreds()
	c.Jira.BaseURL = "http://jira.example.com"
	if _, err := Seal(c, []byte("correct horse battery staple")); err == nil {
		t.Fatal("expected error for non-https Jira")
	}

	c = testCreds()
	c.VPN.Password = "line\ninjection"
	if _, err := Seal(c, []byte("correct horse battery staple")); err == nil {
		t.Fatal("expected error for line break in vpn password")
	}
}

func TestSecretsAreNotPrinted(t *testing.T) {
	c := testCreds()
	for _, s := range []string{
		strings.Join([]string{c.Jira.Password.String(), c.VPN.Password.String()}, ","),
	} {
		if strings.Contains(s, "p@ss") || strings.Contains(s, "vpn") {
			t.Fatalf("secret leaked: %s", s)
		}
	}
}

func TestBuildConfig(t *testing.T) {
	routes, err := parseRoutes([]string{"10.1.2.3/32", "10.20.0.0/16"})
	if err != nil {
		t.Fatal(err)
	}
	cfg := buildConfig("client\nauth-user-pass /etc/pass.txt\n", "/run/vpn/mgmt.sock", routes)
	for _, want := range []string{
		"management /run/vpn/mgmt.sock unix",
		"management-query-passwords",
		"auth-retry none",
		"route-nopull",
		"route 10.1.2.3 255.255.255.255",
		"route 10.20.0.0 255.255.0.0",
	} {
		if !strings.Contains(cfg, want+"\n") {
			t.Errorf("config has no %q", want)
		}
	}
	// Наша auth-user-pass без файла должна идти после пользовательской и перекрыть её
	if strings.LastIndex(cfg, "auth-user-pass\n") < strings.Index(cfg, "auth-user-pass /etc/pass.txt") {
		t.Error("our auth-user-pass does not override the user's one")
	}
}

func TestQuote(t *testing.T) {
	if got := quote(`a"b\c d`); got != `"a\"b\\c d"` {
		t.Fatalf("quote = %s", got)
	}
}

// fakeOpenVPN ведёт себя как sidecar: ждёт конфиг, открывает management-сокет,
// просит логин и пароль, проверяет их и подключается
func fakeOpenVPN(t *testing.T, dir, wantPass string, fail bool) <-chan []string {
	t.Helper()
	got := make(chan []string, 1)
	go func() {
		for {
			if _, err := os.Stat(filepath.Join(dir, tunnelConfigName)); err == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		ln, err := net.Listen("unix", filepath.Join(dir, tunnelSocketName))
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = ln.Close() }()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		var cmds []string
		w := bufio.NewWriter(conn)
		say := func(s string) { _, _ = w.WriteString(s + "\r\n"); _ = w.Flush() }
		say(">INFO:OpenVPN Management Interface Version 5 -- type 'help' for more info")
		sc := bufio.NewScanner(conn)
		for sc.Scan() {
			cmd := sc.Text()
			cmds = append(cmds, cmd)
			switch {
			case cmd == "hold release":
				say("SUCCESS: hold release succeeded")
				say(">PASSWORD:Need 'Auth' username/password")
			case strings.HasPrefix(cmd, "password "):
				if fail || cmd != `password "Auth" `+quote(wantPass) {
					say(">PASSWORD:Verification Failed: 'Auth'")
					continue
				}
				say(">STATE:1700000000,CONNECTED,SUCCESS,10.8.0.2,1.2.3.4,1194,,")
			case cmd == "signal SIGTERM":
				got <- cmds
				return
			}
		}
		got <- cmds
	}()
	return got
}

func TestTunnelConnects(t *testing.T) {
	dir := t.TempDir()
	creds := *testCreds().VPN
	cmds := fakeOpenVPN(t, dir, creds.Password.Reveal(), false)

	sess, err := NewTunnel(dir, 5*time.Second).Up(context.Background(), creds)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, tunnelConfigName)); !os.IsNotExist(err) {
		t.Fatal("config file is still on disk after connect")
	}
	sess.Close()

	got := <-cmds
	want := []string{"state on", "hold release", `username "Auth" "john.doe"`, `password "Auth" ` + quote(creds.Password.Reveal()), "signal SIGTERM"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("commands = %q, want %q", got, want)
	}
}

func TestTunnelAuthFailed(t *testing.T) {
	dir := t.TempDir()
	creds := *testCreds().VPN
	fakeOpenVPN(t, dir, creds.Password.Reveal(), true)

	_, err := NewTunnel(dir, 5*time.Second).Up(context.Background(), creds)
	if !errors.Is(err, ErrVPNAuth) {
		t.Fatalf("error = %v, want ErrVPNAuth", err)
	}
}

func TestTunnelTimeoutWithoutSidecar(t *testing.T) {
	_, err := NewTunnel(t.TempDir(), 300*time.Millisecond).Up(context.Background(), *testCreds().VPN)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want deadline exceeded", err)
	}
}

func TestStopOrphan(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, tunnelSocketName)

	// Сокет без слушателя - мусор, просто удаляется
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	if ul, ok := ln.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	_ = ln.Close()
	if err := stopOrphan(context.Background(), sock); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatal("stale socket was not removed")
	}

	// Живой openvpn получает SIGTERM, sidecar после его выхода убирает сокет
	ln, err = net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		line, _ := bufio.NewReader(conn).ReadString('\n')
		got <- line
		_ = conn.Close()
		_ = ln.Close()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := stopOrphan(ctx, sock); err != nil {
		t.Fatal(err)
	}
	if line := <-got; line != "signal SIGTERM\n" {
		t.Fatalf("orphan got %q", line)
	}
}

func TestRoutesAndBaseURL(t *testing.T) {
	if _, err := parseRoutes([]string{"fd00::1/128"}); err == nil {
		t.Error("IPv6 route accepted")
	}
	c := testCreds()
	c.Jira.BaseURL = "https://example.com/jira"
	if err := c.Validate(); err != nil {
		t.Errorf("context path rejected: %v", err)
	}
	c.Jira.CACert = "not a pem"
	if err := c.Validate(); err == nil {
		t.Error("broken CA accepted")
	}
}

func TestTunnelEncryptedKey(t *testing.T) {
	dir := t.TempDir()
	go func() {
		for {
			if _, err := os.Stat(filepath.Join(dir, tunnelConfigName)); err == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		ln, err := net.Listen("unix", filepath.Join(dir, tunnelSocketName))
		if err != nil {
			return
		}
		defer func() { _ = ln.Close() }()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = conn.Write([]byte(">PASSWORD:Need 'Private Key' password\r\n"))
		_, _ = bufio.NewReader(conn).ReadString('\n')
		time.Sleep(time.Second)
	}()

	_, err := NewTunnel(dir, 5*time.Second).Up(context.Background(), *testCreds().VPN)
	if err == nil || !strings.Contains(err.Error(), "private key") {
		t.Fatalf("error = %v, want encrypted key error", err)
	}
}
