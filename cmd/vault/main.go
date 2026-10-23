// Утилита создаёт зашифрованное хранилище корпоративных доступов для бота.
// Запускай на своей машине и копируй на сервер уже готовый файл: открытые пароли туда не попадают.
//
//	go run ./cmd/vault -out secrets/creds.age -ovpn ~/client.ovpn
//
// Без -ovpn бот не поднимает VPN сам: так удобно, если он запущен там, где корп VPN уже включён
package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/devWaylander/auto-time-report/internal/corp"
	"github.com/devWaylander/auto-time-report/internal/secret"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "ошибка:", err)
		os.Exit(1)
	}
}

func run() error {
	out := flag.String("out", "secrets/creds.age", "куда записать хранилище")
	ovpnPath := flag.String("ovpn", "", "конфиг OpenVPN со встроенными сертификатами; пусто - без VPN")
	flag.Parse()

	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return errors.New("запусти в терминале: пароли вводятся без эха")
	}
	if _, err := os.Stat(*out); err == nil {
		return fmt.Errorf("%s уже существует - удали его сам, если хочешь пересоздать", *out)
	}
	in := bufio.NewReader(os.Stdin)

	fmt.Println("Jira")
	var c corp.Credentials
	c.Jira.BaseURL = ask(in, "  URL (https://jira.example.com): ")
	c.Jira.IP = ask(in, "  IP, если DNS Jira недоступен через туннель (Enter - пропустить): ")
	c.Jira.Username = ask(in, "  Логин: ")
	c.Jira.Password = askSecret("  Пароль: ")
	c.Jira.ExtraFields = ask(in, "  Поля wa_* из запроса браузера (wa_1=…&wa_2=…, Enter - нет): ")
	if caPath := ask(in, "  Файл сертификата корпоративного CA в PEM, если браузер ругается на сертификат Jira (Enter - нет): "); caPath != "" {
		ca, err := os.ReadFile(caPath)
		if err != nil {
			return err
		}
		c.Jira.CACert = string(ca)
	}

	if *ovpnPath != "" {
		cfg, err := os.ReadFile(*ovpnPath)
		if err != nil {
			return err
		}
		fmt.Println("\nOpenVPN")
		vpn := &corp.VPNCredentials{Config: string(cfg)}
		vpn.Username = ask(in, "  Логин: ")
		vpn.Password = askSecret("  Пароль: ")
		routes := ask(in, "  Подсети через туннель, через запятую (лучше только адрес Jira, например 10.1.2.3/32): ")
		for _, r := range strings.Split(routes, ",") {
			if r = strings.TrimSpace(r); r != "" {
				vpn.Routes = append(vpn.Routes, r)
			}
		}
		c.VPN = vpn
	}
	if err := c.Validate(); err != nil {
		return err
	}

	fmt.Printf("\nПароль-фраза: не короче %d символов, не совпадает с доменным паролем, храни в менеджере паролей\n", corp.MinPassphraseLen)
	phrase := []byte(askSecret("  Пароль-фраза: "))
	defer clear(phrase)
	again := []byte(askSecret("  Ещё раз: "))
	defer clear(again)
	if !bytes.Equal(phrase, again) {
		return errors.New("пароль-фразы не совпадают")
	}

	blob, err := corp.Seal(c, phrase)
	if err != nil {
		return err
	}
	// O_EXCL: не перетираем существующее хранилище, даже если оно появилось, пока мы спрашивали
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
	if err != nil {
		return err
	}
	if _, err := f.Write(blob); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Printf("\nГотово: %s (права 0400). Скопируй его на сервер, исходный .ovpn оттуда удали\n", *out)
	return nil
}

func ask(in *bufio.Reader, prompt string) string {
	fmt.Print(prompt)
	line, _ := in.ReadString('\n')
	return strings.TrimSpace(line)
}

func askSecret(prompt string) secret.String {
	fmt.Print(prompt)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return ""
	}
	return secret.String(strings.TrimRight(string(b), "\r\n"))
}
