package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	IMAPHost string
	IMAPPort int
	IMAPUser string
	IMAPPass string
	IMAPTLS  string // "tls" (default), "starttls", "none"

	SMTPHost string
	SMTPPort int
	SMTPUser string
	SMTPPass string
	SMTPTLS  string // "starttls" (default), "tls", "none"

	From string // default From for send

	// Footer is appended to outgoing message bodies with an RFC 3676 sigdash
	// separator. Loaded from INB_FOOTER (or INB_FOOTER_FILE, which wins
	// if both are set). FooterHTML is the optional HTML-specific override used
	// when --html is set; if empty, Footer is escaped and <br>-wrapped instead.
	Footer     string
	FooterHTML string
}

// loadConfig reads env vars, falling back to ~/.inb.env when present.
// Env wins; the dotenv only fills missing values.
func loadConfig() (*Config, error) {
	loadDotenv()

	c := &Config{
		IMAPHost: os.Getenv("INB_IMAP_HOST"),
		IMAPUser: os.Getenv("INB_IMAP_USER"),
		IMAPPass: os.Getenv("INB_IMAP_PASS"),
		IMAPTLS:  strings.ToLower(getenvDefault("INB_IMAP_TLS", "tls")),

		SMTPHost: os.Getenv("INB_SMTP_HOST"),
		SMTPUser: os.Getenv("INB_SMTP_USER"),
		SMTPPass: os.Getenv("INB_SMTP_PASS"),
		SMTPTLS:  strings.ToLower(getenvDefault("INB_SMTP_TLS", "starttls")),

		From: os.Getenv("INB_FROM"),

		Footer:     os.Getenv("INB_FOOTER"),
		FooterHTML: os.Getenv("INB_FOOTER_HTML"),
	}

	if path := os.Getenv("INB_FOOTER_FILE"); path != "" {
		b, err := os.ReadFile(expandHome(path))
		if err != nil {
			return nil, fmt.Errorf("INB_FOOTER_FILE %s: %w", path, err)
		}
		c.Footer = strings.TrimRight(string(b), "\n")
	}

	if p := os.Getenv("INB_IMAP_PORT"); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("INB_IMAP_PORT: %w", err)
		}
		c.IMAPPort = n
	} else {
		if c.IMAPTLS == "tls" {
			c.IMAPPort = 993
		} else {
			c.IMAPPort = 143
		}
	}

	if p := os.Getenv("INB_SMTP_PORT"); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("INB_SMTP_PORT: %w", err)
		}
		c.SMTPPort = n
	} else {
		switch c.SMTPTLS {
		case "tls":
			c.SMTPPort = 465
		case "none":
			c.SMTPPort = 25
		default:
			c.SMTPPort = 587
		}
	}

	return c, nil
}

func (c *Config) requireIMAP() error {
	var missing []string
	if c.IMAPHost == "" {
		missing = append(missing, "INB_IMAP_HOST")
	}
	if c.IMAPUser == "" {
		missing = append(missing, "INB_IMAP_USER")
	}
	if c.IMAPPass == "" {
		missing = append(missing, "INB_IMAP_PASS")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing IMAP credentials: %s (set in env or ~/.inb.env)", strings.Join(missing, ", "))
	}
	switch c.IMAPTLS {
	case "tls", "starttls", "none":
	default:
		return fmt.Errorf("INB_IMAP_TLS must be tls|starttls|none, got %q", c.IMAPTLS)
	}
	return nil
}

func (c *Config) requireSMTP() error {
	var missing []string
	if c.SMTPHost == "" {
		missing = append(missing, "INB_SMTP_HOST")
	}
	if c.SMTPUser == "" {
		missing = append(missing, "INB_SMTP_USER")
	}
	if c.SMTPPass == "" {
		missing = append(missing, "INB_SMTP_PASS")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing SMTP credentials: %s (set in env or ~/.inb.env)", strings.Join(missing, ", "))
	}
	switch c.SMTPTLS {
	case "tls", "starttls", "none":
	default:
		return fmt.Errorf("INB_SMTP_TLS must be tls|starttls|none, got %q", c.SMTPTLS)
	}
	return nil
}

func getenvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// expandHome resolves a leading "~/" to the user's home directory. Other
// paths (absolute, relative, or starting with $VAR) are returned as-is.
func expandHome(path string) string {
	if !strings.HasPrefix(path, "~/") && path != "~" {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if path == "~" {
		return home
	}
	return filepath.Join(home, path[2:])
}

// loadDotenv reads ~/.inb.env and sets each KEY=VALUE pair into the
// environment, but only for keys not already set. Lines starting with # are
// comments. Values may be quoted with ' or ".
func loadDotenv() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	path := filepath.Join(home, ".inb.env")
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := unquoteDotenv(strings.TrimSpace(line[eq+1:]))
		if _, ok := os.LookupEnv(key); ok {
			continue
		}
		_ = os.Setenv(key, val)
	}
}

// unquoteDotenv decodes a dotenv value: double-quoted strings get backslash
// escapes processed (\\, \", \n, \r, \t); single-quoted strings are taken
// literally; bare values are returned as-is.
func unquoteDotenv(s string) string {
	if len(s) < 2 {
		return s
	}
	q := s[0]
	if q != '"' && q != '\'' {
		return s
	}
	if s[len(s)-1] != q {
		return s
	}
	inner := s[1 : len(s)-1]
	if q == '\'' {
		return inner
	}
	var b strings.Builder
	b.Grow(len(inner))
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		if c != '\\' || i+1 >= len(inner) {
			b.WriteByte(c)
			continue
		}
		i++
		switch inner[i] {
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case '\\', '"', '\'':
			b.WriteByte(inner[i])
		default:
			b.WriteByte('\\')
			b.WriteByte(inner[i])
		}
	}
	return b.String()
}
