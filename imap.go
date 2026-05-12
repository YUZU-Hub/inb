package main

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
)

type imapConn struct {
	c   *client.Client
	cfg *Config
}

func dialIMAP(cfg *Config) (*imapConn, error) {
	addr := fmt.Sprintf("%s:%d", cfg.IMAPHost, cfg.IMAPPort)
	timeout := 30 * time.Second

	var (
		c   *client.Client
		err error
	)

	switch cfg.IMAPTLS {
	case "tls":
		dialer := &net.Dialer{Timeout: timeout}
		c, err = client.DialWithDialerTLS(dialer, addr, &tls.Config{ServerName: cfg.IMAPHost})
	case "starttls":
		dialer := &net.Dialer{Timeout: timeout}
		c, err = client.DialWithDialer(dialer, addr)
		if err == nil {
			err = c.StartTLS(&tls.Config{ServerName: cfg.IMAPHost})
		}
	case "none":
		dialer := &net.Dialer{Timeout: timeout}
		c, err = client.DialWithDialer(dialer, addr)
	default:
		return nil, fmt.Errorf("invalid INB_IMAP_TLS=%q", cfg.IMAPTLS)
	}
	if err != nil {
		return nil, fmt.Errorf("imap dial %s: %w", addr, err)
	}

	if err := c.Login(cfg.IMAPUser, cfg.IMAPPass); err != nil {
		_ = c.Logout()
		return nil, fmt.Errorf("imap login as %s: %w", cfg.IMAPUser, err)
	}
	return &imapConn{c: c, cfg: cfg}, nil
}

func (i *imapConn) close() {
	if i == nil || i.c == nil {
		return
	}
	_ = i.c.Logout()
}

// selectFolder opens the folder read/write and returns its status.
func (i *imapConn) selectFolder(name string, readOnly bool) (*imap.MailboxStatus, error) {
	if name == "" {
		name = "INBOX"
	}
	mbox, err := i.c.Select(name, readOnly)
	if err != nil {
		return nil, fmt.Errorf("select %q: %w", name, err)
	}
	return mbox, nil
}

// listFolders returns mailbox names (top-level + nested).
func (i *imapConn) listFolders() ([]*imap.MailboxInfo, error) {
	ch := make(chan *imap.MailboxInfo, 32)
	done := make(chan error, 1)
	go func() { done <- i.c.List("", "*", ch) }()

	var out []*imap.MailboxInfo
	for m := range ch {
		out = append(out, m)
	}
	if err := <-done; err != nil {
		return nil, fmt.Errorf("list folders: %w", err)
	}
	return out, nil
}

// resolveSpecialFolder picks a folder name for a logical destination
// ("archive", "trash", "junk", "sent", "drafts") based on the server's
// folder list. It checks RFC 6154 \All|\Archive|\Trash|\Junk|\Sent|\Drafts
// attributes first, then falls back to common names.
func (i *imapConn) resolveSpecialFolder(kind string) (string, error) {
	wanted := map[string]string{
		"archive": `\Archive`,
		"trash":   `\Trash`,
		"junk":    `\Junk`,
		"sent":    `\Sent`,
		"drafts":  `\Drafts`,
		"all":     `\All`,
	}
	attr, ok := wanted[strings.ToLower(kind)]
	if !ok {
		return "", fmt.Errorf("unknown special folder %q", kind)
	}
	folders, err := i.listFolders()
	if err != nil {
		return "", err
	}
	for _, f := range folders {
		for _, a := range f.Attributes {
			if strings.EqualFold(a, attr) {
				return f.Name, nil
			}
		}
	}
	fallbacks := map[string][]string{
		"archive": {"Archive", "Archives", "[Gmail]/All Mail", "All Mail"},
		"trash":   {"Trash", "Deleted", "Deleted Items", "Deleted Messages", "[Gmail]/Trash"},
		"junk":    {"Junk", "Spam", "[Gmail]/Spam"},
		"sent":    {"Sent", "Sent Items", "Sent Messages", "[Gmail]/Sent Mail"},
		"drafts":  {"Drafts", "[Gmail]/Drafts"},
	}
	for _, f := range folders {
		for _, fb := range fallbacks[strings.ToLower(kind)] {
			if strings.EqualFold(f.Name, fb) {
				return f.Name, nil
			}
		}
	}
	return "", fmt.Errorf("no %s folder found on server", kind)
}

// searchCriteria builds an IMAP SEARCH criteria from filter flags.
type searchFilters struct {
	Since   time.Time
	Before  time.Time
	From    string
	To      string
	Subject string
	Text    string
	Unread  bool
	Flagged bool
}

func (f searchFilters) toCriteria() *imap.SearchCriteria {
	c := imap.NewSearchCriteria()
	if !f.Since.IsZero() {
		c.Since = f.Since
	}
	if !f.Before.IsZero() {
		c.Before = f.Before
	}
	if f.From != "" {
		c.Header.Add("From", f.From)
	}
	if f.To != "" {
		c.Header.Add("To", f.To)
	}
	if f.Subject != "" {
		c.Header.Add("Subject", f.Subject)
	}
	if f.Text != "" {
		c.Text = []string{f.Text}
	}
	if f.Unread {
		c.WithoutFlags = append(c.WithoutFlags, imap.SeenFlag)
	}
	if f.Flagged {
		c.WithFlags = append(c.WithFlags, imap.FlaggedFlag)
	}
	return c
}

// searchUIDs runs UID SEARCH with criteria; returns sorted DESC (newest first).
func (i *imapConn) searchUIDs(f searchFilters) ([]uint32, error) {
	uids, err := i.c.UidSearch(f.toCriteria())
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	// newest first
	for a, b := 0, len(uids)-1; a < b; a, b = a+1, b-1 {
		uids[a], uids[b] = uids[b], uids[a]
	}
	return uids, nil
}

// fetchEnvelopes returns envelope+flags+size+structure for the given UIDs.
func (i *imapConn) fetchEnvelopes(uids []uint32) ([]*imap.Message, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	set := new(imap.SeqSet)
	set.AddNum(uids...)
	items := []imap.FetchItem{
		imap.FetchEnvelope,
		imap.FetchFlags,
		imap.FetchInternalDate,
		imap.FetchRFC822Size,
		imap.FetchBodyStructure,
		imap.FetchUid,
	}
	ch := make(chan *imap.Message, len(uids))
	done := make(chan error, 1)
	go func() { done <- i.c.UidFetch(set, items, ch) }()

	out := make([]*imap.Message, 0, len(uids))
	for m := range ch {
		out = append(out, m)
	}
	if err := <-done; err != nil {
		return nil, fmt.Errorf("fetch envelopes: %w", err)
	}
	// preserve requested order (UIDs are returned in server order)
	idx := make(map[uint32]int, len(uids))
	for k, u := range uids {
		idx[u] = k
	}
	ordered := make([]*imap.Message, len(out))
	n := 0
	for _, m := range out {
		if p, ok := idx[m.Uid]; ok && p < len(ordered) {
			ordered[p] = m
			n++
		}
	}
	// compact (in case of holes)
	result := make([]*imap.Message, 0, n)
	for _, m := range ordered {
		if m != nil {
			result = append(result, m)
		}
	}
	return result, nil
}

// fetchRaw fetches the full RFC822 body of a single UID.
func (i *imapConn) fetchRaw(uid uint32) ([]byte, error) {
	set := new(imap.SeqSet)
	set.AddNum(uid)
	section := &imap.BodySectionName{Peek: true}
	items := []imap.FetchItem{section.FetchItem(), imap.FetchUid}

	ch := make(chan *imap.Message, 1)
	done := make(chan error, 1)
	go func() { done <- i.c.UidFetch(set, items, ch) }()

	var msg *imap.Message
	for m := range ch {
		msg = m
	}
	if err := <-done; err != nil {
		return nil, fmt.Errorf("fetch body uid=%d: %w", uid, err)
	}
	if msg == nil {
		return nil, fmt.Errorf("uid %d not found", uid)
	}
	r := msg.GetBody(section)
	if r == nil {
		return nil, fmt.Errorf("uid %d has no body section", uid)
	}
	return io.ReadAll(r)
}

// markFlag adds or removes flags on a UID.
func (i *imapConn) markFlag(uid uint32, flag string, add bool) error {
	set := new(imap.SeqSet)
	set.AddNum(uid)
	var op imap.FlagsOp = imap.AddFlags
	if !add {
		op = imap.RemoveFlags
	}
	item := imap.FormatFlagsOp(op, true) // .SILENT
	if err := i.c.UidStore(set, item, []interface{}{flag}, nil); err != nil {
		return fmt.Errorf("store flag %s on uid %d: %w", flag, uid, err)
	}
	return nil
}

// moveUID moves (or copies+deletes if MOVE unsupported) a UID to dest folder.
func (i *imapConn) moveUID(uid uint32, dest string) error {
	set := new(imap.SeqSet)
	set.AddNum(uid)

	if err := i.c.UidMove(set, dest); err == nil {
		return nil
	}
	// fallback for servers without RFC 6851 MOVE
	if err := i.c.UidCopy(set, dest); err != nil {
		return fmt.Errorf("copy uid %d to %q: %w", uid, dest, err)
	}
	if err := i.markFlag(uid, imap.DeletedFlag, true); err != nil {
		return err
	}
	if err := i.c.Expunge(nil); err != nil {
		return fmt.Errorf("expunge: %w", err)
	}
	return nil
}

// appendRaw appends a raw RFC822 message into a folder (used for storing
// sent mail in Sent).
func (i *imapConn) appendRaw(folder string, raw []byte, flags []string) error {
	if folder == "" {
		return nil
	}
	if err := i.c.Append(folder, flags, time.Now(), bytes.NewReader(raw)); err != nil {
		return fmt.Errorf("append to %q: %w", folder, err)
	}
	return nil
}
