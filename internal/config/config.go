// Package config loads Rowsmith's runtime configuration from the environment.
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Addr     string // listen address, e.g. ":8080"
	DataDir  string // SQLite store, generated keys, scheduled exports
	LogLevel string

	// PublicURL is the externally visible URL (e.g. https://db.example.com).
	// It drives the base path, Origin checks and cookie attributes.
	PublicURL *url.URL
	BasePath  string // "/" or "/sql/" — always begins and ends with "/"

	MasterKeyFile string // file holding one base64 key per line; first line is current
	MasterKey     string // alternative to MasterKeyFile (base64)

	TrustedProxies []*net.IPNet

	SessionIdle     time.Duration
	SessionAbsolute time.Duration

	// Insecure disables the Secure cookie flag and HSTS. Only for local http development.
	Insecure bool
}

func Load() (*Config, error) {
	c := &Config{
		Addr:            env("ROWSMITH_ADDR", ":8080"),
		DataDir:         env("ROWSMITH_DATA_DIR", "/data"),
		LogLevel:        env("ROWSMITH_LOG_LEVEL", "info"),
		MasterKeyFile:   os.Getenv("ROWSMITH_MASTER_KEY_FILE"),
		MasterKey:       os.Getenv("ROWSMITH_MASTER_KEY"),
		SessionIdle:     durEnv("ROWSMITH_SESSION_IDLE", 8*time.Hour),
		SessionAbsolute: durEnv("ROWSMITH_SESSION_MAX", 72*time.Hour),
		Insecure:        boolEnv("ROWSMITH_INSECURE_COOKIES", false),
		BasePath:        "/",
	}
	if c.MasterKeyFile == "" && c.MasterKey == "" {
		if _, err := os.Stat("/run/secrets/rowsmith_master_key"); err == nil {
			c.MasterKeyFile = "/run/secrets/rowsmith_master_key"
		}
	}

	if raw := os.Getenv("ROWSMITH_PUBLIC_URL"); raw != "" {
		u, err := url.Parse(strings.TrimRight(raw, "/"))
		if err != nil || u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("ROWSMITH_PUBLIC_URL must be an absolute URL, got %q", raw)
		}
		c.PublicURL = u
		c.BasePath = "/" + strings.Trim(u.Path, "/") + "/"
		if c.BasePath == "//" {
			c.BasePath = "/"
		}
	}

	proxies := env("ROWSMITH_TRUSTED_PROXIES", "127.0.0.0/8,::1/128,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16")
	for _, p := range strings.Split(proxies, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		_, n, err := net.ParseCIDR(p)
		if err != nil {
			return nil, fmt.Errorf("ROWSMITH_TRUSTED_PROXIES: %w", err)
		}
		c.TrustedProxies = append(c.TrustedProxies, n)
	}
	return c, nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func boolEnv(k string, def bool) bool {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func durEnv(k string, def time.Duration) time.Duration {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}
