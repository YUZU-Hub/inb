package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/emersion/go-imap"
	"golang.org/x/term"
)

// printAsJSON encodes any value as indented JSON to w.
func printAsJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// terminalWidth returns the current terminal width, or 0 if stdout is not a
// terminal. The COLUMNS env var is honored when set (handy under pipes / tmux
// quirks).
func terminalWidth() int {
	if v := os.Getenv("COLUMNS"); v != "" {
		if n, err := strconvAtoiSafe(v); err == nil && n > 0 {
			return n
		}
	}
	if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 {
		return w
	}
	return 0
}

func strconvAtoiSafe(s string) (int, error) {
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not a number")
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

type listItem struct {
	UID         uint32   `json:"uid"`
	Date        string   `json:"date"`
	From        string   `json:"from"`
	To          []string `json:"to,omitempty"`
	Subject     string   `json:"subject"`
	Size        uint32   `json:"size"`
	Flags       []string `json:"flags"`
	HasAttach   bool     `json:"has_attachments"`
	MessageID   string   `json:"message_id,omitempty"`
	Folder      string   `json:"folder"`
}

func toListItem(m *imap.Message, folder string) listItem {
	li := listItem{UID: m.Uid, Flags: m.Flags, Size: m.Size, Folder: folder}
	if m.Envelope != nil {
		if !m.Envelope.Date.IsZero() {
			li.Date = m.Envelope.Date.Local().Format("2006-01-02 15:04")
		} else if !m.InternalDate.IsZero() {
			li.Date = m.InternalDate.Local().Format("2006-01-02 15:04")
		}
		li.From = formatAddrs(m.Envelope.From)
		for _, a := range m.Envelope.To {
			li.To = append(li.To, formatAddr(a))
		}
		li.Subject = strings.TrimSpace(m.Envelope.Subject)
		li.MessageID = m.Envelope.MessageId
	} else if !m.InternalDate.IsZero() {
		li.Date = m.InternalDate.Local().Format("2006-01-02 15:04")
	}
	li.HasAttach = hasAttachment(m.BodyStructure)
	return li
}

func formatAddr(a *imap.Address) string {
	if a == nil {
		return ""
	}
	mb := a.MailboxName + "@" + a.HostName
	if a.PersonalName != "" {
		return fmt.Sprintf("%s <%s>", a.PersonalName, mb)
	}
	return mb
}

func formatAddrs(as []*imap.Address) string {
	parts := make([]string, 0, len(as))
	for _, a := range as {
		parts = append(parts, formatAddr(a))
	}
	return strings.Join(parts, ", ")
}

func hasAttachment(bs *imap.BodyStructure) bool {
	if bs == nil {
		return false
	}
	if strings.EqualFold(bs.Disposition, "attachment") {
		return true
	}
	if name := bs.Params["name"]; name != "" && !strings.HasPrefix(strings.ToLower(bs.MIMEType), "text") {
		return true
	}
	for _, p := range bs.Parts {
		if hasAttachment(p) {
			return true
		}
	}
	return false
}

func flagSummary(flags []string) string {
	var b strings.Builder
	seen := func(want string) bool {
		for _, f := range flags {
			if strings.EqualFold(f, want) {
				return true
			}
		}
		return false
	}
	if seen(imap.SeenFlag) {
		b.WriteByte('R')
	} else {
		b.WriteByte('U')
	}
	if seen(imap.AnsweredFlag) {
		b.WriteByte('A')
	}
	if seen(imap.FlaggedFlag) {
		b.WriteByte('!')
	}
	if seen(imap.DraftFlag) {
		b.WriteByte('D')
	}
	return b.String()
}

// column describes one printable column in an adaptive table.
type column struct {
	header   string
	values   []string
	flexible bool // shrunk first when total exceeds terminal width
	minWidth int  // floor when shrinking (after header is respected)
	rightAlign bool
}

// layoutWidths returns the per-column width to use given the terminal width.
// Non-flexible columns always get their natural width; flexible columns share
// the remaining space (shrinking proportionally if the table is too wide).
func layoutWidths(cols []column, termWidth int) []int {
	widths := make([]int, len(cols))
	for i, c := range cols {
		nat := runeLen(c.header)
		for _, v := range c.values {
			if l := runeLen(v); l > nat {
				nat = l
			}
		}
		widths[i] = nat
	}
	if termWidth <= 0 {
		return widths
	}
	sep := 2 * (len(cols) - 1)
	total := sep
	for _, w := range widths {
		total += w
	}
	if total <= termWidth {
		return widths
	}
	// Initial proportional shrink of flexible columns.
	excess := total - termWidth
	flexSum := 0
	for i, c := range cols {
		if c.flexible {
			flexSum += widths[i]
		}
	}
	if flexSum > 0 {
		for i, c := range cols {
			if !c.flexible {
				continue
			}
			cut := (widths[i] * excess) / flexSum
			nw := widths[i] - cut
			min := c.minWidth
			if min < runeLen(c.header) {
				min = runeLen(c.header)
			}
			if nw < min {
				nw = min
			}
			widths[i] = nw
		}
	}
	// Iteratively trim 1 char from whichever flexible column is widest,
	// until the table fits or no column can be shrunk further. This cleans
	// up integer-rounding residue from the proportional pass.
	for {
		t := sep
		for _, w := range widths {
			t += w
		}
		if t <= termWidth {
			break
		}
		largest := -1
		for i, c := range cols {
			if !c.flexible {
				continue
			}
			min := c.minWidth
			if min < runeLen(c.header) {
				min = runeLen(c.header)
			}
			if widths[i] <= min {
				continue
			}
			if largest == -1 || widths[i] > widths[largest] {
				largest = i
			}
		}
		if largest == -1 {
			break // can't shrink further; table will overflow
		}
		widths[largest]--
	}
	return widths
}

func runeLen(s string) int { return utf8.RuneCountInString(s) }

// pad truncates with ellipsis or right-pads s to exactly w runes.
func pad(s string, w int, rightAlign bool) string {
	r := runeLen(s)
	if r > w {
		if w <= 1 {
			return string([]rune(s)[:w])
		}
		runes := []rune(s)
		return string(runes[:w-1]) + "…"
	}
	if rightAlign {
		return strings.Repeat(" ", w-r) + s
	}
	return s + strings.Repeat(" ", w-r)
}

// printList writes a plain-text table of messages, adapting column widths to
// the terminal width (or COLUMNS env var). When stdout is not a TTY, columns
// use their natural width with no truncation.
func printList(w io.Writer, items []listItem) {
	uidVals := make([]string, len(items))
	dateVals := make([]string, len(items))
	flagVals := make([]string, len(items))
	fromVals := make([]string, len(items))
	subjVals := make([]string, len(items))
	sizeVals := make([]string, len(items))
	for i, it := range items {
		uidVals[i] = fmt.Sprintf("%d", it.UID)
		dateVals[i] = it.Date
		fs := flagSummary(it.Flags)
		if it.HasAttach {
			fs += "@"
		}
		flagVals[i] = fs
		fromVals[i] = it.From
		subjVals[i] = strings.ReplaceAll(it.Subject, "\n", " ")
		sizeVals[i] = humanSize(int64(it.Size))
	}
	cols := []column{
		{header: "UID", values: uidVals, rightAlign: true},
		{header: "DATE", values: dateVals},
		{header: "FLAGS", values: flagVals},
		{header: "FROM", values: fromVals, flexible: true, minWidth: 6},
		{header: "SUBJECT", values: subjVals, flexible: true, minWidth: 10},
		{header: "SIZE", values: sizeVals, rightAlign: true},
	}
	widths := layoutWidths(cols, terminalWidth())

	writeRow := func(vals []string) {
		parts := make([]string, len(cols))
		for i, v := range vals {
			parts[i] = pad(v, widths[i], cols[i].rightAlign)
		}
		fmt.Fprintln(w, strings.Join(parts, "  "))
	}
	header := make([]string, len(cols))
	for i, c := range cols {
		header[i] = c.header
	}
	writeRow(header)
	for i := range items {
		writeRow([]string{uidVals[i], dateVals[i], flagVals[i], fromVals[i], subjVals[i], sizeVals[i]})
	}
}

// printAttachments writes a plain-text table of attachments adapted to the
// terminal width.
func printAttachments(w io.Writer, items []parsedAttachment) {
	if len(items) == 0 {
		fmt.Fprintln(w, "(no attachments)")
		return
	}
	idxVals := make([]string, len(items))
	nameVals := make([]string, len(items))
	typeVals := make([]string, len(items))
	sizeVals := make([]string, len(items))
	for i, a := range items {
		idxVals[i] = fmt.Sprintf("%d", a.idx)
		nameVals[i] = a.Filename
		typeVals[i] = a.ContentType
		sizeVals[i] = humanSize(int64(a.Size))
	}
	cols := []column{
		{header: "IDX", values: idxVals, rightAlign: true},
		{header: "FILENAME", values: nameVals, flexible: true, minWidth: 12},
		{header: "TYPE", values: typeVals, flexible: true, minWidth: 12},
		{header: "SIZE", values: sizeVals, rightAlign: true},
	}
	widths := layoutWidths(cols, terminalWidth())
	writeRow := func(vals []string) {
		parts := make([]string, len(cols))
		for i, v := range vals {
			parts[i] = pad(v, widths[i], cols[i].rightAlign)
		}
		fmt.Fprintln(w, strings.Join(parts, "  "))
	}
	header := make([]string, len(cols))
	for i, c := range cols {
		header[i] = c.header
	}
	writeRow(header)
	for i := range items {
		writeRow([]string{idxVals[i], nameVals[i], typeVals[i], sizeVals[i]})
	}
}

func printListJSON(w io.Writer, items []listItem) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if items == nil {
		items = []listItem{}
	}
	return enc.Encode(items)
}

func truncate(s string, n int) string {
	if n <= 1 {
		return s
	}
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func humanSize(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%dB", n)
	}
	units := []string{"K", "M", "G", "T"}
	v := float64(n)
	for _, u := range units {
		v /= 1024
		if v < 1024 {
			if v < 10 {
				return fmt.Sprintf("%.1f%s", v, u)
			}
			return fmt.Sprintf("%.0f%s", v, u)
		}
	}
	return fmt.Sprintf("%.0fP", v/1024)
}

func parseDate(s string) (time.Time, error) {
	layouts := []string{"2006-01-02", "2006-01-02 15:04", time.RFC3339, "01/02/2006"}
	var lastErr error
	for _, l := range layouts {
		if t, err := time.ParseInLocation(l, s, time.Local); err == nil {
			return t, nil
		} else {
			lastErr = err
		}
	}
	return time.Time{}, lastErr
}

// printReadPlain formats a parsed message for the terminal. Truncates body if
// it exceeds maxBytes (set to 0 to disable).
func printReadPlain(w io.Writer, p *parsedMessage, maxBytes int) {
	fmt.Fprintf(w, "From:       %s\n", p.From)
	if len(p.To) > 0 {
		fmt.Fprintf(w, "To:         %s\n", strings.Join(p.To, ", "))
	}
	if len(p.Cc) > 0 {
		fmt.Fprintf(w, "Cc:         %s\n", strings.Join(p.Cc, ", "))
	}
	fmt.Fprintf(w, "Date:       %s\n", p.Date)
	fmt.Fprintf(w, "Subject:    %s\n", p.Subject)
	if p.MessageID != "" {
		fmt.Fprintf(w, "Message-ID: %s\n", p.MessageID)
	}
	if len(p.Attachments) > 0 {
		fmt.Fprintf(w, "Attachments:\n")
		for _, a := range p.Attachments {
			fmt.Fprintf(w, "  [%d] %s (%s, %s)\n", a.idx, a.Filename, a.ContentType, humanSize(int64(a.Size)))
		}
	}
	fmt.Fprintln(w, strings.Repeat("-", 72))
	body := p.TextBody
	if body == "" && p.HTMLBody != "" {
		body = "[HTML body — use --html to view raw, or --raw to dump full message]\n\n" + stripHTML(p.HTMLBody)
	}
	if maxBytes > 0 && len(body) > maxBytes {
		truncatedAt := maxBytes
		fmt.Fprint(w, body[:truncatedAt])
		fmt.Fprintf(w, "\n\n[…truncated %d bytes — use --full to see entire body]\n", len(body)-truncatedAt)
		return
	}
	fmt.Fprint(w, body)
	if !strings.HasSuffix(body, "\n") {
		fmt.Fprintln(w)
	}
}

func printReadJSON(w io.Writer, p *parsedMessage, includeBody bool, maxBytes int) error {
	type out struct {
		Headers     map[string]string  `json:"headers,omitempty"`
		From        string             `json:"from"`
		To          []string           `json:"to,omitempty"`
		Cc          []string           `json:"cc,omitempty"`
		Subject     string             `json:"subject"`
		Date        string             `json:"date"`
		MessageID   string             `json:"message_id,omitempty"`
		Text        string             `json:"text,omitempty"`
		HTML        string             `json:"html,omitempty"`
		Truncated   bool               `json:"truncated,omitempty"`
		Attachments []parsedAttachment `json:"attachments,omitempty"`
	}
	o := out{
		From: p.From, To: p.To, Cc: p.Cc, Subject: p.Subject, Date: p.Date,
		MessageID: p.MessageID, Attachments: p.Attachments,
	}
	if includeBody {
		o.Text = p.TextBody
		o.HTML = p.HTMLBody
		if maxBytes > 0 {
			if len(o.Text) > maxBytes {
				o.Text = o.Text[:maxBytes]
				o.Truncated = true
			}
			if len(o.HTML) > maxBytes {
				o.HTML = o.HTML[:maxBytes]
				o.Truncated = true
			}
		}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(o)
}

// stripHTML is a very lightweight tag-stripper for displaying HTML-only mails
// in plain mode. Not a full HTML parser — good enough for a first look.
func stripHTML(s string) string {
	var b strings.Builder
	inTag := false
	for _, r := range s {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
		case !inTag:
			b.WriteRune(r)
		}
	}
	// collapse runs of blank lines
	out := b.String()
	for strings.Contains(out, "\n\n\n") {
		out = strings.ReplaceAll(out, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(out) + "\n"
}
