package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/emersion/go-imap"
)

const version = "0.1.0"

func main() {
	if len(os.Args) < 2 {
		printUsage(os.Stderr)
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "help", "-h", "--help":
		printUsage(os.Stdout)
		return
	case "version", "-v", "--version":
		fmt.Println(version)
		return
	case "setup":
		if err := cmdSetup(args); err != nil {
			fail(err)
		}
		return
	}

	cfg, err := loadConfig()
	if err != nil {
		fail(err)
	}

	var runErr error
	switch cmd {
	case "list", "ls":
		runErr = cmdList(cfg, args)
	case "search":
		// search is just list with --text default; users can still use list --search
		runErr = cmdList(cfg, append([]string{"--require-search"}, args...))
	case "read", "show":
		runErr = cmdRead(cfg, args)
	case "send":
		runErr = cmdSend(cfg, args)
	case "mark":
		runErr = cmdMark(cfg, args)
	case "delete", "rm":
		runErr = cmdMove(cfg, args, "trash")
	case "archive":
		runErr = cmdMove(cfg, args, "archive")
	case "move", "mv":
		runErr = cmdMove(cfg, args, "")
	case "folders":
		runErr = cmdFolders(cfg, args)
	case "attachments":
		runErr = cmdAttachments(cfg, args)
	case "save-attachment":
		runErr = cmdSaveAttachment(cfg, args)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", cmd)
		printUsage(os.Stderr)
		os.Exit(2)
	}
	if runErr != nil {
		fail(runErr)
	}
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "inb: %v\n", err)
	os.Exit(1)
}

func printUsage(w io.Writer) {
	fmt.Fprintf(w, `inb v%s — the agent inbox.

Usage: inb <command> [flags]

Commands:
  setup                 interactive wizard: detect provider, test creds, write ~/.inb.env
  list, ls              list messages (filters: --since/--before/--from/--subject/--unread/--search)
  read, show <UID>      read a message (headers + body, truncated by default)
  send                  compose & send mail (--to/--subject/--body, --attach repeatable)
  search <query>        full-text search (shortcut for list --search)
  mark <UID>            change flags (--read/--unread/--flag/--unflag)
  delete, rm <UID>      move to Trash
  archive <UID>         move to Archive (auto-detected)
  move, mv <UID>        move to a folder (--to <folder>)
  folders               list IMAP folders
  attachments <UID>     list attachments of a message
  save-attachment <UID> save one attachment (--name <file> or --index N, --out <path>)
  help, version

Global flags:
  --folder <name>       IMAP folder (default INBOX)
  --json                JSON output instead of plain text

Credentials (env vars; also reads ~/.inb.env if present):
  INB_IMAP_HOST, INB_IMAP_PORT, INB_IMAP_USER, INB_IMAP_PASS, INB_IMAP_TLS=tls|starttls|none
  INB_SMTP_HOST, INB_SMTP_PORT, INB_SMTP_USER, INB_SMTP_PASS, INB_SMTP_TLS=starttls|tls|none
  INB_FROM          default From: address for send
  INB_FOOTER        optional signature appended to outgoing mail (sigdash separator)
  INB_FOOTER_FILE   path to a file whose contents are used as the footer (wins over INB_FOOTER)
  INB_FOOTER_HTML   optional HTML footer used when sending --html (else INB_FOOTER is auto-escaped)

Run "inb <command> --help" for command-specific flags.
`, version)
}

// -------- list --------

type listFlags struct {
	folder        string
	limit         int
	since, before string
	from          string
	to            string
	subject       string
	search        string
	unread        bool
	flagged       bool
	asJSON        bool
	requireSearch bool // for `inb search` enforces a query
}

func cmdList(cfg *Config, args []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var lf listFlags
	fs.StringVar(&lf.folder, "folder", "INBOX", "IMAP folder")
	fs.IntVar(&lf.limit, "limit", 20, "max messages to return")
	fs.StringVar(&lf.since, "since", "", "messages on or after date (YYYY-MM-DD)")
	fs.StringVar(&lf.before, "before", "", "messages before date (YYYY-MM-DD)")
	fs.StringVar(&lf.from, "from", "", "filter by From header (substring)")
	fs.StringVar(&lf.to, "to", "", "filter by To header (substring)")
	fs.StringVar(&lf.subject, "subject", "", "filter by Subject (substring)")
	fs.StringVar(&lf.search, "search", "", "server-side full-text search")
	fs.BoolVar(&lf.unread, "unread", false, "only unread messages")
	fs.BoolVar(&lf.flagged, "flagged", false, "only flagged messages")
	fs.BoolVar(&lf.asJSON, "json", false, "JSON output")
	help := fs.Bool("help", false, "show help")

	// support a hidden flag from `inb search`
	for i, a := range args {
		if a == "--require-search" {
			lf.requireSearch = true
			args = append(args[:i], args[i+1:]...)
			break
		}
	}

	if err := fs.Parse(args); err != nil {
		return helpError(fs, err)
	}
	if *help {
		fs.SetOutput(os.Stdout)
		fs.Usage()
		return nil
	}
	// `inb search <query…>` — positional args become the search text
	if lf.requireSearch && lf.search == "" {
		if fs.NArg() == 0 {
			return errors.New("usage: inb search <query>")
		}
		lf.search = strings.Join(fs.Args(), " ")
	}

	if err := cfg.requireIMAP(); err != nil {
		return err
	}
	ic, err := dialIMAP(cfg)
	if err != nil {
		return err
	}
	defer ic.close()

	if _, err := ic.selectFolder(lf.folder, true); err != nil {
		return err
	}

	filters := searchFilters{From: lf.from, To: lf.to, Subject: lf.subject, Text: lf.search, Unread: lf.unread, Flagged: lf.flagged}
	if lf.since != "" {
		t, err := parseDate(lf.since)
		if err != nil {
			return fmt.Errorf("--since: %w", err)
		}
		filters.Since = t
	}
	if lf.before != "" {
		t, err := parseDate(lf.before)
		if err != nil {
			return fmt.Errorf("--before: %w", err)
		}
		filters.Before = t
	}

	uids, err := ic.searchUIDs(filters)
	if err != nil {
		return err
	}
	if lf.limit > 0 && len(uids) > lf.limit {
		uids = uids[:lf.limit]
	}
	msgs, err := ic.fetchEnvelopes(uids)
	if err != nil {
		return err
	}
	items := make([]listItem, 0, len(msgs))
	for _, m := range msgs {
		items = append(items, toListItem(m, lf.folder))
	}
	if lf.asJSON {
		return printListJSON(os.Stdout, items)
	}
	printList(os.Stdout, items)
	return nil
}

// -------- read --------

func cmdRead(cfg *Config, args []string) error {
	fs := flag.NewFlagSet("read", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	folder := fs.String("folder", "INBOX", "IMAP folder")
	maxBytes := fs.Int("max-bytes", 32*1024, "truncate body at N bytes (0 = no limit)")
	full := fs.Bool("full", false, "no truncation (equivalent to --max-bytes=0)")
	raw := fs.Bool("raw", false, "print raw RFC822 message")
	headersOnly := fs.Bool("headers-only", false, "skip body")
	markRead := fs.Bool("mark-read", false, "mark message as \\Seen after fetching")
	html := fs.Bool("html", false, "prefer HTML body in plain output")
	asJSON := fs.Bool("json", false, "JSON output")
	help := fs.Bool("help", false, "show help")

	if err := fs.Parse(args); err != nil {
		return helpError(fs, err)
	}
	if *help {
		fs.SetOutput(os.Stdout)
		fs.Usage()
		return nil
	}
	if fs.NArg() < 1 {
		return errors.New("usage: inb read <UID> [flags]")
	}
	uid, err := parseUID(fs.Arg(0))
	if err != nil {
		return err
	}
	if *full {
		*maxBytes = 0
	}

	if err := cfg.requireIMAP(); err != nil {
		return err
	}
	ic, err := dialIMAP(cfg)
	if err != nil {
		return err
	}
	defer ic.close()

	if _, err := ic.selectFolder(*folder, !*markRead); err != nil {
		return err
	}
	body, err := ic.fetchRaw(uid)
	if err != nil {
		return err
	}
	if *markRead {
		_ = ic.markFlag(uid, imap.SeenFlag, true)
	}
	if *raw {
		_, err := os.Stdout.Write(body)
		return err
	}
	p, err := parseRFC822(body)
	if err != nil {
		return err
	}
	if *html && p.HTMLBody != "" {
		p.TextBody = p.HTMLBody // forces plain printer to show HTML raw
		p.HTMLBody = ""
	}
	if *headersOnly {
		p.TextBody = ""
		p.HTMLBody = ""
	}
	if *asJSON {
		return printReadJSON(os.Stdout, p, !*headersOnly, *maxBytes)
	}
	printReadPlain(os.Stdout, p, *maxBytes)
	return nil
}

// -------- send --------

func cmdSend(cfg *Config, args []string) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	from := fs.String("from", cfg.From, "From address (default $INB_FROM)")
	to := fs.String("to", "", "comma-separated To addresses (required)")
	cc := fs.String("cc", "", "comma-separated Cc addresses")
	bcc := fs.String("bcc", "", "comma-separated Bcc addresses")
	replyTo := fs.String("reply-to", "", "Reply-To address")
	subject := fs.String("subject", "", "Subject (required)")
	body := fs.String("body", "", "body text (use - to read from stdin)")
	bodyFile := fs.String("body-file", "", "read body from file")
	html := fs.Bool("html", false, "body is HTML")
	var attach multiString
	fs.Var(&attach, "attach", "path to attachment (repeatable)")
	saveSent := fs.Bool("save-sent", true, "append to Sent folder via IMAP after send")
	footer := fs.String("footer", "", "footer text (overrides $INB_FOOTER for this call)")
	footerHTML := fs.String("footer-html", "", "HTML footer for --html sends (overrides $INB_FOOTER_HTML)")
	footerFile := fs.String("footer-file", "", "read footer from file (overrides $INB_FOOTER / $INB_FOOTER_FILE)")
	noFooter := fs.Bool("no-footer", false, "do not append any configured footer for this call")
	asJSON := fs.Bool("json", false, "JSON output on success")
	help := fs.Bool("help", false, "show help")

	if err := fs.Parse(args); err != nil {
		return helpError(fs, err)
	}
	if *help {
		fs.SetOutput(os.Stdout)
		fs.Usage()
		return nil
	}

	bodyText, err := resolveBody(*body, *bodyFile)
	if err != nil {
		return err
	}

	ftr := cfg.Footer
	ftrHTML := cfg.FooterHTML
	if *footerFile != "" {
		b, err := os.ReadFile(*footerFile)
		if err != nil {
			return fmt.Errorf("read %s: %w", *footerFile, err)
		}
		ftr = strings.TrimRight(string(b), "\n")
		ftrHTML = ""
	}
	if *footer != "" {
		ftr = *footer
	}
	if *footerHTML != "" {
		ftrHTML = *footerHTML
	}

	msg := &sendMsg{
		From:        *from,
		To:          splitAddrs(*to),
		Cc:          splitAddrs(*cc),
		Bcc:         splitAddrs(*bcc),
		ReplyTo:     *replyTo,
		Subject:     *subject,
		Body:        bodyText,
		HTML:        *html,
		Attachments: []string(attach),
		Footer:      ftr,
		FooterHTML:  ftrHTML,
		NoFooter:    *noFooter,
	}

	if err := cfg.requireSMTP(); err != nil {
		return err
	}
	raw, err := sendMail(cfg, msg)
	if err != nil {
		return err
	}

	// Best-effort append to Sent over IMAP (Gmail does this server-side; many
	// providers don't, hence this fallback). Failures go to stderr so they
	// don't go unnoticed, but never abort the send.
	appended := false
	if *saveSent && cfg.IMAPHost != "" && cfg.IMAPUser != "" {
		saveErr := func() error {
			ic, err := dialIMAP(cfg)
			if err != nil {
				return fmt.Errorf("connect IMAP: %w", err)
			}
			defer ic.close()
			sent, err := ic.resolveSpecialFolder("sent")
			if err != nil {
				return fmt.Errorf("locate Sent folder: %w", err)
			}
			return ic.appendRaw(sent, raw, []string{imap.SeenFlag})
		}()
		if saveErr == nil {
			appended = true
		} else {
			fmt.Fprintf(os.Stderr, "warning: could not save to Sent: %v\n", saveErr)
		}
	}

	if *asJSON {
		fmt.Fprintf(os.Stdout, "{\"ok\":true,\"recipients\":%d,\"bytes\":%d,\"saved_to_sent\":%t}\n",
			len(msg.To)+len(msg.Cc)+len(msg.Bcc), len(raw), appended)
		return nil
	}
	fmt.Fprintf(os.Stdout, "sent: %d recipient(s), %d bytes", len(msg.To)+len(msg.Cc)+len(msg.Bcc), len(raw))
	if appended {
		fmt.Fprintf(os.Stdout, " (saved to Sent)")
	}
	fmt.Fprintln(os.Stdout)
	return nil
}

// -------- mark --------

func cmdMark(cfg *Config, args []string) error {
	fs := flag.NewFlagSet("mark", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	folder := fs.String("folder", "INBOX", "IMAP folder")
	read := fs.Bool("read", false, "mark \\Seen")
	unread := fs.Bool("unread", false, "remove \\Seen")
	flagged := fs.Bool("flag", false, "mark \\Flagged")
	unflagged := fs.Bool("unflag", false, "remove \\Flagged")
	help := fs.Bool("help", false, "show help")

	if err := fs.Parse(args); err != nil {
		return helpError(fs, err)
	}
	if *help {
		fs.SetOutput(os.Stdout)
		fs.Usage()
		return nil
	}
	if fs.NArg() < 1 {
		return errors.New("usage: inb mark <UID> [--read|--unread|--flag|--unflag]")
	}
	uid, err := parseUID(fs.Arg(0))
	if err != nil {
		return err
	}

	type op struct {
		flag string
		add  bool
		name string
	}
	var ops []op
	if *read {
		ops = append(ops, op{imap.SeenFlag, true, "read"})
	}
	if *unread {
		ops = append(ops, op{imap.SeenFlag, false, "unread"})
	}
	if *flagged {
		ops = append(ops, op{imap.FlaggedFlag, true, "flag"})
	}
	if *unflagged {
		ops = append(ops, op{imap.FlaggedFlag, false, "unflag"})
	}
	if len(ops) == 0 {
		return errors.New("specify at least one of --read/--unread/--flag/--unflag")
	}

	if err := cfg.requireIMAP(); err != nil {
		return err
	}
	ic, err := dialIMAP(cfg)
	if err != nil {
		return err
	}
	defer ic.close()
	if _, err := ic.selectFolder(*folder, false); err != nil {
		return err
	}
	for _, o := range ops {
		if err := ic.markFlag(uid, o.flag, o.add); err != nil {
			return err
		}
	}
	names := make([]string, 0, len(ops))
	for _, o := range ops {
		names = append(names, o.name)
	}
	fmt.Fprintf(os.Stdout, "ok: uid=%d %s\n", uid, strings.Join(names, ","))
	return nil
}

// -------- move/delete/archive --------

func cmdMove(cfg *Config, args []string, special string) error {
	fs := flag.NewFlagSet("move", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	folder := fs.String("folder", "INBOX", "source IMAP folder")
	dest := fs.String("to", "", "destination folder")
	help := fs.Bool("help", false, "show help")

	if err := fs.Parse(args); err != nil {
		return helpError(fs, err)
	}
	if *help {
		fs.SetOutput(os.Stdout)
		fs.Usage()
		return nil
	}
	if fs.NArg() < 1 {
		return errors.New("usage: inb (move|delete|archive) <UID> [--to <folder>]")
	}
	uid, err := parseUID(fs.Arg(0))
	if err != nil {
		return err
	}

	if err := cfg.requireIMAP(); err != nil {
		return err
	}
	ic, err := dialIMAP(cfg)
	if err != nil {
		return err
	}
	defer ic.close()

	destName := *dest
	if destName == "" && special != "" {
		destName, err = ic.resolveSpecialFolder(special)
		if err != nil {
			return err
		}
	}
	if destName == "" {
		return errors.New("--to <folder> is required for move")
	}

	if _, err := ic.selectFolder(*folder, false); err != nil {
		return err
	}
	if err := ic.moveUID(uid, destName); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "ok: uid=%d -> %s\n", uid, destName)
	return nil
}

// -------- folders --------

func cmdFolders(cfg *Config, args []string) error {
	fs := flag.NewFlagSet("folders", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := fs.Bool("json", false, "JSON output")
	help := fs.Bool("help", false, "show help")

	if err := fs.Parse(args); err != nil {
		return helpError(fs, err)
	}
	if *help {
		fs.SetOutput(os.Stdout)
		fs.Usage()
		return nil
	}

	if err := cfg.requireIMAP(); err != nil {
		return err
	}
	ic, err := dialIMAP(cfg)
	if err != nil {
		return err
	}
	defer ic.close()
	folders, err := ic.listFolders()
	if err != nil {
		return err
	}
	if *asJSON {
		type f struct {
			Name       string   `json:"name"`
			Delimiter  string   `json:"delimiter"`
			Attributes []string `json:"attributes"`
		}
		out := make([]f, 0, len(folders))
		for _, fl := range folders {
			out = append(out, f{Name: fl.Name, Delimiter: fl.Delimiter, Attributes: fl.Attributes})
		}
		return jsonEncode(os.Stdout, out)
	}
	for _, fl := range folders {
		fmt.Fprintf(os.Stdout, "%s\n", fl.Name)
	}
	return nil
}

// -------- attachments --------

func cmdAttachments(cfg *Config, args []string) error {
	fs := flag.NewFlagSet("attachments", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	folder := fs.String("folder", "INBOX", "IMAP folder")
	asJSON := fs.Bool("json", false, "JSON output")
	help := fs.Bool("help", false, "show help")

	if err := fs.Parse(args); err != nil {
		return helpError(fs, err)
	}
	if *help {
		fs.SetOutput(os.Stdout)
		fs.Usage()
		return nil
	}
	if fs.NArg() < 1 {
		return errors.New("usage: inb attachments <UID>")
	}
	uid, err := parseUID(fs.Arg(0))
	if err != nil {
		return err
	}

	if err := cfg.requireIMAP(); err != nil {
		return err
	}
	ic, err := dialIMAP(cfg)
	if err != nil {
		return err
	}
	defer ic.close()
	if _, err := ic.selectFolder(*folder, true); err != nil {
		return err
	}
	raw, err := ic.fetchRaw(uid)
	if err != nil {
		return err
	}
	p, err := parseRFC822(raw)
	if err != nil {
		return err
	}
	if *asJSON {
		return jsonEncode(os.Stdout, p.Attachments)
	}
	printAttachments(os.Stdout, p.Attachments)
	return nil
}

// -------- save-attachment --------

func cmdSaveAttachment(cfg *Config, args []string) error {
	fs := flag.NewFlagSet("save-attachment", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	folder := fs.String("folder", "INBOX", "IMAP folder")
	name := fs.String("name", "", "attachment filename (case-insensitive)")
	idx := fs.Int("index", 0, "1-based attachment index (alternative to --name)")
	out := fs.String("out", "", "output file path or directory (default: current dir)")
	help := fs.Bool("help", false, "show help")

	if err := fs.Parse(args); err != nil {
		return helpError(fs, err)
	}
	if *help {
		fs.SetOutput(os.Stdout)
		fs.Usage()
		return nil
	}
	if fs.NArg() < 1 {
		return errors.New("usage: inb save-attachment <UID> (--name <file> | --index N) [--out <path>]")
	}
	uid, err := parseUID(fs.Arg(0))
	if err != nil {
		return err
	}
	if *name == "" && *idx == 0 {
		return errors.New("specify --name <file> or --index <N>")
	}

	if err := cfg.requireIMAP(); err != nil {
		return err
	}
	ic, err := dialIMAP(cfg)
	if err != nil {
		return err
	}
	defer ic.close()
	if _, err := ic.selectFolder(*folder, true); err != nil {
		return err
	}
	raw, err := ic.fetchRaw(uid)
	if err != nil {
		return err
	}
	filename, data, err := readAttachment(raw, *idx, *name)
	if err != nil {
		return err
	}

	outPath := *out
	if outPath == "" {
		outPath = filename
	} else if fi, err := os.Stat(outPath); err == nil && fi.IsDir() {
		outPath = filepath.Join(outPath, filename)
	}
	if err := os.WriteFile(outPath, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", outPath, err)
	}
	fmt.Fprintf(os.Stdout, "saved: %s (%s)\n", outPath, humanSize(int64(len(data))))
	return nil
}

// -------- helpers --------

func helpError(fs *flag.FlagSet, err error) error {
	if errors.Is(err, flag.ErrHelp) {
		fs.SetOutput(os.Stdout)
		fs.Usage()
		return nil
	}
	return err
}

func parseUID(s string) (uint32, error) {
	n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid UID %q: %w", s, err)
	}
	return uint32(n), nil
}

func splitAddrs(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func resolveBody(body, bodyFile string) (string, error) {
	if body == "-" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("read stdin: %w", err)
		}
		return string(b), nil
	}
	if bodyFile != "" {
		b, err := os.ReadFile(bodyFile)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", bodyFile, err)
		}
		return string(b), nil
	}
	return body, nil
}

type multiString []string

func (m *multiString) String() string     { return strings.Join(*m, ",") }
func (m *multiString) Set(v string) error { *m = append(*m, v); return nil }

func jsonEncode(w io.Writer, v any) error {
	return printAsJSON(w, v)
}
