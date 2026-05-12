package main

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"html"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type sendMsg struct {
	From        string
	To          []string
	Cc          []string
	Bcc         []string
	ReplyTo     string
	Subject     string
	Body        string
	HTML        bool
	Attachments []string // file paths
	Headers     map[string]string

	// Footer is appended to Body with an RFC 3676 sigdash separator unless
	// NoFooter is true. For HTML bodies, FooterHTML wins; if empty, Footer is
	// HTML-escaped and <br>-wrapped automatically.
	Footer     string
	FooterHTML string
	NoFooter   bool
}

// build returns the RFC 5322 message bytes plus the SMTP envelope recipient list.
func (m *sendMsg) build() (raw []byte, rcpts []string, err error) {
	if m.From == "" {
		return nil, nil, fmt.Errorf("From is required (set --from or INB_FROM)")
	}
	if len(m.To) == 0 && len(m.Cc) == 0 && len(m.Bcc) == 0 {
		return nil, nil, fmt.Errorf("at least one recipient (--to/--cc/--bcc) required")
	}
	if m.Subject == "" {
		return nil, nil, fmt.Errorf("--subject is required")
	}

	rcpts = append(rcpts, m.To...)
	rcpts = append(rcpts, m.Cc...)
	rcpts = append(rcpts, m.Bcc...)

	bodyText := m.Body
	if !m.NoFooter {
		bodyText = appendFooter(bodyText, m.Footer, m.FooterHTML, m.HTML)
	}

	h := textproto.MIMEHeader{}
	h.Set("From", m.From)
	if len(m.To) > 0 {
		h.Set("To", strings.Join(m.To, ", "))
	}
	if len(m.Cc) > 0 {
		h.Set("Cc", strings.Join(m.Cc, ", "))
	}
	if m.ReplyTo != "" {
		h.Set("Reply-To", m.ReplyTo)
	}
	h.Set("Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	h.Set("Date", time.Now().Format(time.RFC1123Z))
	h.Set("Message-ID", makeMessageID(m.From))
	h.Set("MIME-Version", "1.0")
	for k, v := range m.Headers {
		h.Set(k, v)
	}

	var buf bytes.Buffer

	if len(m.Attachments) == 0 {
		ctype := "text/plain; charset=utf-8"
		if m.HTML {
			ctype = "text/html; charset=utf-8"
		}
		h.Set("Content-Type", ctype)
		h.Set("Content-Transfer-Encoding", "8bit")
		writeHeaders(&buf, h)
		buf.WriteString("\r\n")
		buf.WriteString(bodyText)
		return buf.Bytes(), rcpts, nil
	}

	// multipart/mixed: body + attachments
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	h.Set("Content-Type", `multipart/mixed; boundary="`+mw.Boundary()+`"`)

	bodyHdr := textproto.MIMEHeader{}
	if m.HTML {
		bodyHdr.Set("Content-Type", "text/html; charset=utf-8")
	} else {
		bodyHdr.Set("Content-Type", "text/plain; charset=utf-8")
	}
	bodyHdr.Set("Content-Transfer-Encoding", "8bit")
	bp, err := mw.CreatePart(bodyHdr)
	if err != nil {
		return nil, nil, err
	}
	if _, err := bp.Write([]byte(bodyText)); err != nil {
		return nil, nil, err
	}
	for _, p := range m.Attachments {
		if err := writeAttachment(mw, p); err != nil {
			return nil, nil, fmt.Errorf("attach %s: %w", p, err)
		}
	}
	if err := mw.Close(); err != nil {
		return nil, nil, err
	}

	writeHeaders(&buf, h)
	buf.WriteString("\r\n")
	buf.Write(body.Bytes())
	return buf.Bytes(), rcpts, nil
}

func writeHeaders(w io.Writer, h textproto.MIMEHeader) {
	ordered := []string{"From", "To", "Cc", "Reply-To", "Subject", "Date", "Message-ID", "MIME-Version", "Content-Type", "Content-Transfer-Encoding"}
	written := map[string]bool{}
	for _, k := range ordered {
		if v := h.Get(k); v != "" {
			fmt.Fprintf(w, "%s: %s\r\n", k, v)
			written[textproto.CanonicalMIMEHeaderKey(k)] = true
		}
	}
	for k, vv := range h {
		if written[k] {
			continue
		}
		for _, v := range vv {
			fmt.Fprintf(w, "%s: %s\r\n", k, v)
		}
	}
}

func writeAttachment(mw *multipart.Writer, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	name := filepath.Base(path)
	ctype := mime.TypeByExtension(filepath.Ext(name))
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Type", ctype+`; name="`+name+`"`)
	h.Set("Content-Disposition", `attachment; filename="`+name+`"`)
	h.Set("Content-Transfer-Encoding", "base64")
	pw, err := mw.CreatePart(h)
	if err != nil {
		return err
	}
	encoded := base64.StdEncoding.EncodeToString(data)
	// wrap at 76 chars per RFC 2045
	for i := 0; i < len(encoded); i += 76 {
		end := i + 76
		if end > len(encoded) {
			end = len(encoded)
		}
		if _, err := pw.Write([]byte(encoded[i:end])); err != nil {
			return err
		}
		if _, err := pw.Write([]byte("\r\n")); err != nil {
			return err
		}
	}
	return nil
}

func makeMessageID(from string) string {
	host := "localhost"
	if addr, err := mail.ParseAddress(from); err == nil {
		if i := strings.IndexByte(addr.Address, '@'); i >= 0 {
			host = addr.Address[i+1:]
		}
	}
	return fmt.Sprintf("<%d.%d@%s>", time.Now().UnixNano(), os.Getpid(), host)
}

// sendMail dials the SMTP server and sends the message. Returns the raw
// RFC822 bytes so callers can append to Sent if they want.
func sendMail(cfg *Config, m *sendMsg) ([]byte, error) {
	if m.From == "" {
		m.From = cfg.From
	}
	raw, rcpts, err := m.build()
	if err != nil {
		return nil, err
	}

	envFrom, err := envelopeAddr(m.From)
	if err != nil {
		return nil, fmt.Errorf("invalid From %q: %w", m.From, err)
	}
	envRcpts := make([]string, 0, len(rcpts))
	for _, r := range rcpts {
		e, err := envelopeAddr(r)
		if err != nil {
			return nil, fmt.Errorf("invalid recipient %q: %w", r, err)
		}
		envRcpts = append(envRcpts, e)
	}

	addr := fmt.Sprintf("%s:%d", cfg.SMTPHost, cfg.SMTPPort)
	dialer := &net.Dialer{Timeout: 30 * time.Second}

	var conn net.Conn
	switch cfg.SMTPTLS {
	case "tls":
		tlsCfg := &tls.Config{ServerName: cfg.SMTPHost}
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, tlsCfg)
	default:
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("smtp dial %s: %w", addr, err)
	}
	defer conn.Close()

	c, err := smtp.NewClient(conn, cfg.SMTPHost)
	if err != nil {
		return nil, fmt.Errorf("smtp client: %w", err)
	}
	defer c.Close()

	if cfg.SMTPTLS == "starttls" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return nil, fmt.Errorf("server %s does not advertise STARTTLS", addr)
		}
		if err := c.StartTLS(&tls.Config{ServerName: cfg.SMTPHost}); err != nil {
			return nil, fmt.Errorf("starttls: %w", err)
		}
	}

	if cfg.SMTPUser != "" {
		auth := smtp.PlainAuth("", cfg.SMTPUser, cfg.SMTPPass, cfg.SMTPHost)
		if err := c.Auth(auth); err != nil {
			return nil, fmt.Errorf("smtp auth as %s: %w", cfg.SMTPUser, err)
		}
	}

	if err := c.Mail(envFrom); err != nil {
		return nil, fmt.Errorf("MAIL FROM %s: %w", envFrom, err)
	}
	for _, r := range envRcpts {
		if err := c.Rcpt(r); err != nil {
			return nil, fmt.Errorf("RCPT TO %s: %w", r, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return nil, fmt.Errorf("DATA: %w", err)
	}
	if _, err := w.Write(raw); err != nil {
		return nil, fmt.Errorf("write data: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("close data: %w", err)
	}
	_ = c.Quit()
	return raw, nil
}

// testSMTP performs a handshake + AUTH against the configured SMTP server
// without sending a message. Used by `inb setup` connection tests.
func testSMTP(cfg *Config) error {
	addr := fmt.Sprintf("%s:%d", cfg.SMTPHost, cfg.SMTPPort)
	dialer := &net.Dialer{Timeout: 30 * time.Second}

	var (
		conn net.Conn
		err  error
	)
	switch cfg.SMTPTLS {
	case "tls":
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: cfg.SMTPHost})
	default:
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	defer conn.Close()

	c, err := smtp.NewClient(conn, cfg.SMTPHost)
	if err != nil {
		return fmt.Errorf("client: %w", err)
	}
	defer c.Close()

	if cfg.SMTPTLS == "starttls" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return fmt.Errorf("server %s does not advertise STARTTLS", addr)
		}
		if err := c.StartTLS(&tls.Config{ServerName: cfg.SMTPHost}); err != nil {
			return fmt.Errorf("starttls: %w", err)
		}
	}
	if cfg.SMTPUser != "" {
		auth := smtp.PlainAuth("", cfg.SMTPUser, cfg.SMTPPass, cfg.SMTPHost)
		if err := c.Auth(auth); err != nil {
			return fmt.Errorf("auth as %s: %w", cfg.SMTPUser, err)
		}
	}
	_ = c.Quit()
	return nil
}

func envelopeAddr(s string) (string, error) {
	a, err := mail.ParseAddress(s)
	if err != nil {
		return "", err
	}
	return a.Address, nil
}

// appendFooter appends footer text to body with an RFC 3676 sigdash separator
// ("\n\n-- \n" for text, "<br><br>-- <br>\n" for HTML). For HTML bodies,
// footerHTML wins if non-empty; otherwise plain footer is HTML-escaped and
// newline-wrapped. Empty footer returns body unchanged.
func appendFooter(body, footer, footerHTML string, isHTML bool) string {
	if isHTML {
		f := footerHTML
		if f == "" && footer != "" {
			f = html.EscapeString(footer)
			f = strings.ReplaceAll(f, "\n", "<br>\n")
		}
		if f == "" {
			return body
		}
		return body + "<br><br>-- <br>\n" + f
	}
	if footer == "" {
		return body
	}
	body = strings.TrimRight(body, "\n")
	return body + "\n\n-- \n" + footer
}
