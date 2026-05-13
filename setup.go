package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/mail"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/term"
)

type providerPreset struct {
	Name     string
	IMAPHost string
	IMAPPort int
	IMAPTLS  string
	SMTPHost string
	SMTPPort int
	SMTPTLS  string
	Note     string
}

// providerPresets maps an email domain (lowercase) to default server settings.
// Aliases (e.g. googlemail.com → gmail) are normalized in detectProvider.
var providerPresets = map[string]providerPreset{
	"gmail.com": {
		Name: "Gmail",
		IMAPHost: "imap.gmail.com", IMAPPort: 993, IMAPTLS: "tls",
		SMTPHost: "smtp.gmail.com", SMTPPort: 587, SMTPTLS: "starttls",
		Note: "Requires an app password — https://myaccount.google.com/apppasswords",
	},
	"icloud.com": {
		Name: "iCloud",
		IMAPHost: "imap.mail.me.com", IMAPPort: 993, IMAPTLS: "tls",
		SMTPHost: "smtp.mail.me.com", SMTPPort: 587, SMTPTLS: "starttls",
		Note: "Requires an app-specific password — https://account.apple.com",
	},
	"fastmail.com": {
		Name: "Fastmail",
		IMAPHost: "imap.fastmail.com", IMAPPort: 993, IMAPTLS: "tls",
		SMTPHost: "smtp.fastmail.com", SMTPPort: 465, SMTPTLS: "tls",
		Note: "Create an app password at fastmail.com/settings/security/devicekeys",
	},
	"outlook.com": {
		Name: "Outlook/Office365",
		IMAPHost: "outlook.office365.com", IMAPPort: 993, IMAPTLS: "tls",
		SMTPHost: "smtp.office365.com", SMTPPort: 587, SMTPTLS: "starttls",
		Note: "May require modern auth / app password depending on tenant policy",
	},
	"yahoo.com": {
		Name: "Yahoo",
		IMAPHost: "imap.mail.yahoo.com", IMAPPort: 993, IMAPTLS: "tls",
		SMTPHost: "smtp.mail.yahoo.com", SMTPPort: 587, SMTPTLS: "starttls",
		Note: "Requires an app password from Yahoo account security settings",
	},
	"proton.me": {
		Name: "Proton Mail (Bridge)",
		IMAPHost: "127.0.0.1", IMAPPort: 1143, IMAPTLS: "starttls",
		SMTPHost: "127.0.0.1", SMTPPort: 1025, SMTPTLS: "starttls",
		Note: "Requires Proton Mail Bridge running locally with bridge credentials",
	},
}

var domainAliases = map[string]string{
	"googlemail.com": "gmail.com",
	"me.com":         "icloud.com",
	"mac.com":        "icloud.com",
	"hotmail.com":    "outlook.com",
	"live.com":       "outlook.com",
	"office365.com":  "outlook.com",
	"fastmail.fm":    "fastmail.com",
	"pm.me":          "proton.me",
	"protonmail.com": "proton.me",
}

func detectProvider(email string) (providerPreset, bool) {
	at := strings.LastIndexByte(email, '@')
	if at < 0 {
		return providerPreset{}, false
	}
	dom := strings.ToLower(strings.TrimSpace(email[at+1:]))
	if a, ok := domainAliases[dom]; ok {
		dom = a
	}
	p, ok := providerPresets[dom]
	return p, ok
}

func cmdSetup(args []string) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	email := fs.String("email", "", "email address (used for provider detection & defaults)")
	imapHost := fs.String("imap-host", "", "IMAP host")
	imapPort := fs.Int("imap-port", 0, "IMAP port (0 = auto from TLS mode)")
	imapUser := fs.String("imap-user", "", "IMAP username (defaults to --email)")
	imapPass := fs.String("imap-pass", "", "IMAP password (omit to prompt)")
	imapTLS := fs.String("imap-tls", "", "tls|starttls|none")
	smtpHost := fs.String("smtp-host", "", "SMTP host")
	smtpPort := fs.Int("smtp-port", 0, "SMTP port (0 = auto)")
	smtpUser := fs.String("smtp-user", "", "SMTP username (defaults to --email)")
	smtpPass := fs.String("smtp-pass", "", "SMTP password (omit to prompt)")
	smtpTLS := fs.String("smtp-tls", "", "starttls|tls|none")
	from := fs.String("from", "", "default From: address")
	footer := fs.String("footer", "", "signature/footer for outgoing mail")
	footerFile := fs.String("footer-file", "", "read footer from file (overrides --footer)")
	out := fs.String("out", "", "output path (default ~/.inb.env)")
	force := fs.Bool("force", false, "overwrite existing file without prompting")
	printOnly := fs.Bool("print", false, "print to stdout instead of writing to file")
	skipTest := fs.Bool("no-test", false, "skip IMAP/SMTP connection tests")
	testOnly := fs.Bool("test", false, "only test current env credentials; do not write")
	help := fs.Bool("help", false, "show help")

	if err := fs.Parse(args); err != nil {
		return helpError(fs, err)
	}
	if *help {
		fs.SetOutput(os.Stdout)
		fs.Usage()
		return nil
	}

	// --test: just verify whatever loadConfig() returns.
	if *testOnly {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return runConnectionTests(cfg, os.Stdout)
	}

	interactive := isTerminal(os.Stdin)
	rd := bufio.NewReader(os.Stdin)

	cfg := &Config{From: *from}

	// Step 1: email + provider detection
	if *email == "" && interactive {
		fmt.Fprintln(os.Stdout, "inb setup")
		fmt.Fprintln(os.Stdout, "-------------")
		fmt.Fprintln(os.Stdout, "Configures IMAP/SMTP. Credentials are written to ~/.inb.env (chmod 600).")
		fmt.Fprintln(os.Stdout)
		v, err := prompt(rd, "Email address", "")
		if err != nil {
			return err
		}
		*email = v
	}
	if *email != "" {
		if _, err := mail.ParseAddress(*email); err != nil {
			return fmt.Errorf("invalid --email %q: %w", *email, err)
		}
	}
	preset, presetOK := detectProvider(*email)
	if presetOK && interactive {
		fmt.Fprintf(os.Stdout, "\nDetected provider: %s\n", preset.Name)
		if preset.Note != "" {
			fmt.Fprintf(os.Stdout, "  Note: %s\n", preset.Note)
		}
		fmt.Fprintln(os.Stdout)
	}

	// IMAP
	cfg.IMAPHost = pickStr(*imapHost, preset.IMAPHost)
	cfg.IMAPTLS = pickStr(strings.ToLower(*imapTLS), preset.IMAPTLS, "tls")
	cfg.IMAPPort = pickInt(*imapPort, preset.IMAPPort, defaultIMAPPort(cfg.IMAPTLS))
	cfg.IMAPUser = pickStr(*imapUser, *email)

	if interactive {
		fmt.Fprintln(os.Stdout, "IMAP")
		var err error
		if cfg.IMAPHost, err = prompt(rd, "  host", cfg.IMAPHost); err != nil {
			return err
		}
		if cfg.IMAPTLS, err = promptChoice(rd, "  TLS mode", cfg.IMAPTLS, []string{"tls", "starttls", "none"}); err != nil {
			return err
		}
		if cfg.IMAPPort == 0 || (*imapPort == 0 && preset.IMAPPort == 0) {
			cfg.IMAPPort = defaultIMAPPort(cfg.IMAPTLS)
		}
		if cfg.IMAPPort, err = promptInt(rd, "  port", cfg.IMAPPort); err != nil {
			return err
		}
		if cfg.IMAPUser, err = prompt(rd, "  username", cfg.IMAPUser); err != nil {
			return err
		}
	}
	cfg.IMAPPass = *imapPass
	if cfg.IMAPPass == "" {
		if !interactive {
			return errors.New("--imap-pass is required when stdin is not a TTY (or use --print/--test interactively)")
		}
		p, err := promptPassword(rd, "  password (hidden): ")
		if err != nil {
			return err
		}
		cfg.IMAPPass = p
	}

	// SMTP
	if interactive {
		fmt.Fprintln(os.Stdout, "\nSMTP")
	}
	cfg.SMTPHost = pickStr(*smtpHost, preset.SMTPHost)
	cfg.SMTPTLS = pickStr(strings.ToLower(*smtpTLS), preset.SMTPTLS, "starttls")
	cfg.SMTPPort = pickInt(*smtpPort, preset.SMTPPort, defaultSMTPPort(cfg.SMTPTLS))
	cfg.SMTPUser = pickStr(*smtpUser, cfg.IMAPUser)

	if interactive {
		var err error
		if cfg.SMTPHost, err = prompt(rd, "  host", cfg.SMTPHost); err != nil {
			return err
		}
		if cfg.SMTPTLS, err = promptChoice(rd, "  TLS mode", cfg.SMTPTLS, []string{"starttls", "tls", "none"}); err != nil {
			return err
		}
		if cfg.SMTPPort == 0 || (*smtpPort == 0 && preset.SMTPPort == 0) {
			cfg.SMTPPort = defaultSMTPPort(cfg.SMTPTLS)
		}
		if cfg.SMTPPort, err = promptInt(rd, "  port", cfg.SMTPPort); err != nil {
			return err
		}
		if cfg.SMTPUser, err = prompt(rd, "  username", cfg.SMTPUser); err != nil {
			return err
		}
	}
	cfg.SMTPPass = *smtpPass
	if cfg.SMTPPass == "" {
		if interactive {
			fmt.Fprint(os.Stdout, "  password (hidden, blank = same as IMAP): ")
			p, err := readPassword()
			if err != nil {
				return err
			}
			if p == "" {
				cfg.SMTPPass = cfg.IMAPPass
				fmt.Fprintln(os.Stdout, "  using IMAP password")
			} else {
				cfg.SMTPPass = p
			}
		} else {
			cfg.SMTPPass = cfg.IMAPPass
		}
	}

	// From
	if cfg.From == "" {
		cfg.From = *email
	}
	if interactive {
		var defaultName, defaultAddr string
		if a, err := mail.ParseAddress(cfg.From); err == nil {
			defaultName = a.Name
			defaultAddr = a.Address
		} else {
			defaultAddr = cfg.From
		}
		name, err := prompt(rd, "\nDisplay name (shown as sender; blank for bare address)", defaultName)
		if err != nil {
			return err
		}
		addr, err := prompt(rd, "Default From address", defaultAddr)
		if err != nil {
			return err
		}
		addr = strings.TrimSpace(addr)
		name = strings.TrimSpace(name)
		if name == "" {
			cfg.From = addr
		} else {
			cfg.From = (&mail.Address{Name: name, Address: addr}).String()
		}
		if _, err := mail.ParseAddress(cfg.From); err != nil {
			return fmt.Errorf("invalid From address: %w", err)
		}
	}

	// Footer
	cfg.Footer = *footer
	if *footerFile != "" {
		b, err := os.ReadFile(expandHome(*footerFile))
		if err != nil {
			return fmt.Errorf("read --footer-file %s: %w", *footerFile, err)
		}
		cfg.Footer = strings.TrimRight(string(b), "\n")
	}
	if interactive && cfg.Footer == "" {
		ok, err := promptYesNo(rd, "\nAdd a signature/footer to outgoing mail?", false)
		if err != nil {
			return err
		}
		if ok {
			fmt.Fprintln(os.Stdout, "  Enter footer lines. End with a blank line.")
			var lines []string
			for {
				fmt.Fprint(os.Stdout, "  > ")
				line, err := rd.ReadString('\n')
				if err != nil && err != io.EOF {
					return err
				}
				line = strings.TrimRight(line, "\r\n")
				if line == "" {
					break
				}
				lines = append(lines, line)
			}
			cfg.Footer = strings.Join(lines, "\n")
		}
	}

	if err := cfg.requireIMAP(); err != nil {
		return err
	}
	if err := cfg.requireSMTP(); err != nil {
		return err
	}

	// Test connections
	if !*skipTest {
		if err := runConnectionTests(cfg, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "\ninb: connection test failed: %v\n", err)
			if interactive {
				ok, _ := promptYesNo(rd, "Save anyway?", false)
				if !ok {
					return errors.New("aborted; fix the credentials and rerun `inb setup`")
				}
			} else {
				return errors.New("connection test failed (use --no-test to skip)")
			}
		}
	}

	// Output
	content := renderEnvFile(cfg)
	if *printOnly {
		fmt.Fprint(os.Stdout, content)
		return nil
	}

	outPath := *out
	if outPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("UserHomeDir: %w", err)
		}
		outPath = filepath.Join(home, ".inb.env")
	}
	if _, err := os.Stat(outPath); err == nil && !*force {
		if !interactive {
			return fmt.Errorf("%s exists (use --force to overwrite)", outPath)
		}
		ok, err := promptYesNo(rd, fmt.Sprintf("\n%s exists. Overwrite?", outPath), false)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("aborted; nothing written")
		}
	}
	if err := writeSecure(outPath, []byte(content)); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "\nwrote %s (mode 0600)\n", outPath)
	return nil
}

func runConnectionTests(cfg *Config, w io.Writer) error {
	fmt.Fprintln(w, "\nTesting connections…")

	fmt.Fprint(w, "  IMAP: ")
	ic, err := dialIMAP(cfg)
	if err != nil {
		fmt.Fprintln(w, "FAIL")
		return fmt.Errorf("IMAP: %w", err)
	}
	st, err := ic.selectFolder("INBOX", true)
	ic.close()
	if err != nil {
		fmt.Fprintln(w, "FAIL")
		return fmt.Errorf("IMAP SELECT INBOX: %w", err)
	}
	fmt.Fprintf(w, "ok (INBOX: %d messages)\n", st.Messages)

	fmt.Fprint(w, "  SMTP: ")
	if err := testSMTP(cfg); err != nil {
		fmt.Fprintln(w, "FAIL")
		return fmt.Errorf("SMTP: %w", err)
	}
	fmt.Fprintln(w, "ok (authenticated)")
	return nil
}

func renderEnvFile(cfg *Config) string {
	var b strings.Builder
	fmt.Fprintln(&b, "# inb credentials — written by `inb setup`")
	fmt.Fprintln(&b, "# Env vars take precedence over this file.")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "INB_IMAP_HOST=%s\n", cfg.IMAPHost)
	fmt.Fprintf(&b, "INB_IMAP_PORT=%d\n", cfg.IMAPPort)
	fmt.Fprintf(&b, "INB_IMAP_USER=%s\n", cfg.IMAPUser)
	fmt.Fprintf(&b, "INB_IMAP_PASS=%s\n", quoteValue(cfg.IMAPPass))
	fmt.Fprintf(&b, "INB_IMAP_TLS=%s\n", cfg.IMAPTLS)
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "INB_SMTP_HOST=%s\n", cfg.SMTPHost)
	fmt.Fprintf(&b, "INB_SMTP_PORT=%d\n", cfg.SMTPPort)
	fmt.Fprintf(&b, "INB_SMTP_USER=%s\n", cfg.SMTPUser)
	fmt.Fprintf(&b, "INB_SMTP_PASS=%s\n", quoteValue(cfg.SMTPPass))
	fmt.Fprintf(&b, "INB_SMTP_TLS=%s\n", cfg.SMTPTLS)
	if cfg.From != "" {
		fmt.Fprintln(&b)
		fmt.Fprintf(&b, "INB_FROM=%s\n", quoteValue(cfg.From))
	}
	if cfg.Footer != "" {
		fmt.Fprintln(&b)
		fmt.Fprintf(&b, "INB_FOOTER=%s\n", quoteValue(cfg.Footer))
	}
	if cfg.FooterHTML != "" {
		fmt.Fprintf(&b, "INB_FOOTER_HTML=%s\n", quoteValue(cfg.FooterHTML))
	}
	return b.String()
}

// quoteValue encodes a value for the dotenv file. Uses double-quotes with
// backslash escapes when the value contains characters the loader needs to
// special-case (whitespace, '#', quotes, backslash, newlines).
func quoteValue(s string) string {
	if s == "" {
		return ""
	}
	needs := false
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r', '#', '"', '\'', '\\':
			needs = true
		}
		if needs {
			break
		}
	}
	if !needs {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func writeSecure(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".inb.env.*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("rename %s: %w", tmp.Name(), err)
	}
	return os.Chmod(path, 0o600)
}

// ---------- prompt helpers ----------

func prompt(rd *bufio.Reader, label, def string) (string, error) {
	if def != "" {
		fmt.Fprintf(os.Stdout, "%s [%s]: ", label, def)
	} else {
		fmt.Fprintf(os.Stdout, "%s: ", label)
	}
	line, err := rd.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return def, nil
	}
	return line, nil
}

func promptInt(rd *bufio.Reader, label string, def int) (int, error) {
	s, err := prompt(rd, label, strconv.Itoa(def))
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("%s: not a number: %q", label, s)
	}
	return n, nil
}

func promptChoice(rd *bufio.Reader, label, def string, choices []string) (string, error) {
	for {
		s, err := prompt(rd, fmt.Sprintf("%s (%s)", label, strings.Join(choices, "/")), def)
		if err != nil {
			return "", err
		}
		s = strings.ToLower(strings.TrimSpace(s))
		for _, c := range choices {
			if s == c {
				return s, nil
			}
		}
		fmt.Fprintf(os.Stdout, "  please choose one of: %s\n", strings.Join(choices, ", "))
	}
}

func promptYesNo(rd *bufio.Reader, label string, def bool) (bool, error) {
	d := "y/N"
	if def {
		d = "Y/n"
	}
	for {
		fmt.Fprintf(os.Stdout, "%s [%s]: ", label, d)
		line, err := rd.ReadString('\n')
		if err != nil && err != io.EOF {
			return false, err
		}
		line = strings.ToLower(strings.TrimSpace(line))
		if line == "" {
			return def, nil
		}
		switch line {
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
	}
}

func promptPassword(_ *bufio.Reader, label string) (string, error) {
	fmt.Fprint(os.Stdout, label)
	return readPassword()
}

func readPassword() (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		// non-interactive: read one line of plain stdin
		rd := bufio.NewReader(os.Stdin)
		s, err := rd.ReadString('\n')
		if err != nil && err != io.EOF {
			return "", err
		}
		return strings.TrimRight(s, "\r\n"), nil
	}
	b, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stdout)
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	return string(b), nil
}

func isTerminal(f *os.File) bool {
	return term.IsTerminal(int(f.Fd()))
}

func pickStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func pickInt(vals ...int) int {
	for _, v := range vals {
		if v != 0 {
			return v
		}
	}
	return 0
}

func defaultIMAPPort(tls string) int {
	switch tls {
	case "tls":
		return 993
	default:
		return 143
	}
}

func defaultSMTPPort(tls string) int {
	switch tls {
	case "tls":
		return 465
	case "none":
		return 25
	default:
		return 587
	}
}
