package gateway

import (
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	Mode, Host, PublicOrigin, ProxySecret, IdentityHeader, DatabasePath, CredentialsFile string
	Port                                                                                 int
	TrustedProxyIPs                                                                      []string
	RateLimit                                                                            int
}

func envDefault(k, v string) string {
	if x := os.Getenv(k); x != "" {
		return x
	}
	return v
}
func LoadConfig() (Config, error) {
	home, _ := os.UserHomeDir()
	c := Config{Mode: envDefault("AUTH_MODE", "pangolin"), Host: envDefault("HOST", "127.0.0.1"), PublicOrigin: os.Getenv("PUBLIC_ORIGIN"), ProxySecret: os.Getenv("PANGOLIN_PROXY_SECRET"), IdentityHeader: strings.ToLower(os.Getenv("PANGOLIN_IDENTITY_HEADER")), DatabasePath: envDefault("DATABASE_PATH", "./data/gateway.sqlite"), CredentialsFile: envDefault("FEISHU_CREDENTIALS_FILE", filepath.Join(home, ".config/feishu/credentials.json")), RateLimit: 60}
	var err error
	c.Port, err = strconv.Atoi(envDefault("PORT", "8787"))
	if err != nil {
		return c, err
	}
	c.TrustedProxyIPs = strings.Split(envDefault("TRUSTED_PROXY_IPS", "127.0.0.1,::1"), ",")
	for i := range c.TrustedProxyIPs {
		c.TrustedProxyIPs[i] = strings.TrimSpace(c.TrustedProxyIPs[i])
	}
	if c.PublicOrigin == "" && c.Mode == "local" {
		c.PublicOrigin = "http://" + net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	}
	return c, c.Validate()
}
func (c Config) Validate() error {
	if c.Mode != "local" && c.Mode != "pangolin" {
		return errors.New("AUTH_MODE must be local or pangolin")
	}
	if c.Port < 1 || c.Port > 65535 {
		return errors.New("invalid PORT")
	}
	u, e := url.Parse(c.PublicOrigin)
	if e != nil || u.Scheme == "" || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return errors.New("PUBLIC_ORIGIN must be a complete origin without a path or trailing slash")
	}
	if c.Mode == "local" {
		if c.Host != "127.0.0.1" && c.Host != "::1" {
			return errors.New("local mode must bind loopback")
		}
		ip := net.ParseIP(u.Hostname())
		if (ip == nil || !ip.IsLoopback()) && u.Hostname() != "localhost" {
			return errors.New("local PUBLIC_ORIGIN must be loopback")
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return errors.New("invalid local scheme")
		}
	} else {
		if u.Scheme != "https" {
			return errors.New("Pangolin mode requires HTTPS PUBLIC_ORIGIN")
		}
		if len(c.ProxySecret) < 32 {
			return errors.New("PANGOLIN_PROXY_SECRET must be at least 32 characters")
		}
	}
	if len(c.TrustedProxyIPs) == 0 {
		return errors.New("TRUSTED_PROXY_IPS is required")
	}
	for _, s := range c.TrustedProxyIPs {
		if net.ParseIP(s) == nil {
			return errors.New("TRUSTED_PROXY_IPS must contain exact IP addresses")
		}
	}
	if c.IdentityHeader != "" {
		if !strings.HasPrefix(c.IdentityHeader, "x-") {
			return errors.New("identity header must start with x-")
		}
		for _, r := range c.IdentityHeader {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				return errors.New("invalid identity header")
			}
		}
	}
	return nil
}
