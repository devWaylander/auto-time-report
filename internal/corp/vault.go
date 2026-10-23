package corp

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"

	"filippo.io/age"

	"github.com/devWaylander/auto-time-report/internal/secret"
)

// ErrBadPassphrase - пароль-фраза не подошла к хранилищу
var ErrBadPassphrase = errors.New("wrong passphrase")

// MinPassphraseLen - минимальная длина пароль-фразы; блоб защищён только ей
const MinPassphraseLen = 16

// Credentials - содержимое зашифрованного хранилища
type Credentials struct {
	Jira JiraCredentials `json:"jira"`
	// VPN не задан - туннель не поднимается (локальный запуск под уже включённым корп VPN)
	VPN *VPNCredentials `json:"vpn,omitempty"`
}

type JiraCredentials struct {
	BaseURL     string        `json:"base_url"`
	IP          string        `json:"ip,omitempty"`
	Username    string        `json:"username"`
	Password    secret.String `json:"password"`
	ExtraFields string        `json:"extra_fields,omitempty"`
	// PEM корпоративного CA, если сертификат Jira выпущен не публичным центром
	CACert string `json:"ca_cert,omitempty"`
}

// RootCAs - пул с корпоративным CA или nil, если он не задан
func (j JiraCredentials) RootCAs() (*x509.CertPool, error) {
	if strings.TrimSpace(j.CACert) == "" {
		return nil, nil
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(j.CACert)) {
		return nil, errors.New("jira ca_cert is not a valid PEM certificate")
	}
	return pool, nil
}

type VPNCredentials struct {
	Username string        `json:"username"`
	Password secret.String `json:"password"`
	// Содержимое .ovpn целиком, со встроенными сертификатами
	Config string   `json:"config"`
	Routes []string `json:"routes"`
}

// MarshalJSON нужен, чтобы secret.String записался в хранилище как есть, а не как "***"
func (c Credentials) MarshalJSON() ([]byte, error) {
	type jira struct {
		BaseURL     string `json:"base_url"`
		IP          string `json:"ip,omitempty"`
		Username    string `json:"username"`
		Password    string `json:"password"`
		ExtraFields string `json:"extra_fields,omitempty"`
		CACert      string `json:"ca_cert,omitempty"`
	}
	type vpn struct {
		Username string   `json:"username"`
		Password string   `json:"password"`
		Config   string   `json:"config"`
		Routes   []string `json:"routes"`
	}
	out := struct {
		Jira jira `json:"jira"`
		VPN  *vpn `json:"vpn,omitempty"`
	}{
		Jira: jira{
			BaseURL: c.Jira.BaseURL, IP: c.Jira.IP, Username: c.Jira.Username,
			Password: c.Jira.Password.Reveal(), ExtraFields: c.Jira.ExtraFields, CACert: c.Jira.CACert,
		},
	}
	if c.VPN != nil {
		out.VPN = &vpn{
			Username: c.VPN.Username, Password: c.VPN.Password.Reveal(),
			Config: c.VPN.Config, Routes: c.VPN.Routes,
		}
	}
	return json.Marshal(out)
}

// Validate проверяет, что из хранилища можно собрать рабочий доступ
func (c Credentials) Validate() error {
	var errs []error
	u, err := url.Parse(c.Jira.BaseURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		errs = append(errs, fmt.Errorf("jira base_url must look like https://jira.example.com or https://example.com/jira, got %q", c.Jira.BaseURL))
	}
	if c.Jira.IP != "" && net.ParseIP(c.Jira.IP) == nil {
		errs = append(errs, fmt.Errorf("jira ip %q is not an IP address", c.Jira.IP))
	}
	if c.Jira.Username == "" || c.Jira.Password == "" {
		errs = append(errs, errors.New("jira username and password are required"))
	}
	if _, err := c.Jira.RootCAs(); err != nil {
		errs = append(errs, err)
	}
	if _, err := url.ParseQuery(c.Jira.ExtraFields); err != nil {
		errs = append(errs, fmt.Errorf("jira extra_fields: %w", err))
	}
	if c.VPN != nil {
		if c.VPN.Username == "" || c.VPN.Password == "" || strings.TrimSpace(c.VPN.Config) == "" {
			errs = append(errs, errors.New("vpn username, password and config are required"))
		}
		if strings.ContainsAny(c.VPN.Username+c.VPN.Password.Reveal(), "\r\n") {
			errs = append(errs, errors.New("vpn username and password must not contain line breaks"))
		}
		if _, err := parseRoutes(c.VPN.Routes); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Seal шифрует доступы пароль-фразой (age, scrypt)
func Seal(c Credentials, passphrase []byte) ([]byte, error) {
	if len(passphrase) < MinPassphraseLen {
		return nil, fmt.Errorf("passphrase must be at least %d characters", MinPassphraseLen)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	plain, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	defer clear(plain)

	recipient, err := age.NewScryptRecipient(string(passphrase))
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, recipient)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(plain); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Open расшифровывает хранилище. Go не даёт гарантированно затереть строки, поэтому цель -
// держать доступы в памяти секунды на время отправки, а не защититься от root на сервере
func Open(path string, passphrase []byte) (Credentials, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return Credentials{}, fmt.Errorf("read creds: %w", err)
	}

	identity, err := age.NewScryptIdentity(string(passphrase))
	if err != nil {
		return Credentials{}, err
	}
	r, err := age.Decrypt(bytes.NewReader(blob), identity)
	if err != nil {
		var noMatch *age.NoIdentityMatchError
		if errors.As(err, &noMatch) {
			return Credentials{}, ErrBadPassphrase
		}
		return Credentials{}, fmt.Errorf("decrypt creds: %w", err)
	}
	plain, err := io.ReadAll(r)
	if err != nil {
		return Credentials{}, fmt.Errorf("decrypt creds: %w", err)
	}
	defer clear(plain)

	var c Credentials
	if err := json.Unmarshal(plain, &c); err != nil {
		return Credentials{}, errors.New("creds are not valid JSON")
	}
	if err := c.Validate(); err != nil {
		return Credentials{}, err
	}
	return c, nil
}

func parseRoutes(routes []string) ([]*net.IPNet, error) {
	if len(routes) == 0 {
		return nil, errors.New("vpn routes are required: list only Jira's address, e.g. 10.1.2.3/32")
	}
	var nets []*net.IPNet
	for _, r := range routes {
		_, n, err := net.ParseCIDR(strings.TrimSpace(r))
		if err != nil {
			return nil, fmt.Errorf("vpn route %q: %w", r, err)
		}
		if n.IP.To4() == nil {
			return nil, fmt.Errorf("vpn route %s: only IPv4 routes are supported", n)
		}
		if ones, _ := n.Mask.Size(); ones == 0 {
			return nil, fmt.Errorf("vpn route %s would send all traffic through the VPN", n)
		}
		nets = append(nets, n)
	}
	return nets, nil
}
