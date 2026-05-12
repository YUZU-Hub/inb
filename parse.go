package main

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	_ "github.com/emersion/go-message/charset"
	gomail "github.com/emersion/go-message/mail"
)

type parsedMessage struct {
	Headers     map[string]string
	From        string
	To          []string
	Cc          []string
	Subject     string
	Date        string
	MessageID   string
	TextBody    string
	HTMLBody    string
	Attachments []parsedAttachment
}

type parsedAttachment struct {
	Filename    string
	ContentType string
	Size        int
	idx         int // 1-based index for retrieval
}

// parseRFC822 parses a raw RFC822 message and returns headers, text/html body
// and attachment metadata. Attachment contents are NOT loaded into memory; use
// readAttachment with the same raw bytes to extract one.
func parseRFC822(raw []byte) (*parsedMessage, error) {
	r, err := gomail.CreateReader(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("parse message: %w", err)
	}
	defer r.Close()

	m := &parsedMessage{Headers: map[string]string{}}
	h := r.Header

	if addrs, err := h.AddressList("From"); err == nil && len(addrs) > 0 {
		m.From = addrs[0].String()
	}
	if addrs, err := h.AddressList("To"); err == nil {
		for _, a := range addrs {
			m.To = append(m.To, a.String())
		}
	}
	if addrs, err := h.AddressList("Cc"); err == nil {
		for _, a := range addrs {
			m.Cc = append(m.Cc, a.String())
		}
	}
	if v, err := h.Subject(); err == nil {
		m.Subject = v
	}
	if v, err := h.Date(); err == nil && !v.IsZero() {
		m.Date = v.Format("2006-01-02 15:04:05 -0700")
	}
	if v, err := h.MessageID(); err == nil {
		m.MessageID = v
	}
	hdrFields := h.Fields()
	for hdrFields.Next() {
		k := hdrFields.Key()
		v, err := hdrFields.Text()
		if err != nil {
			v = hdrFields.Value()
		}
		// keep last value for duplicates (Received: etc.)
		m.Headers[k] = v
	}

	idx := 0
	for {
		p, err := r.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("next part: %w", err)
		}
		switch ph := p.Header.(type) {
		case *gomail.InlineHeader:
			ctype, _, _ := ph.ContentType()
			body, _ := io.ReadAll(p.Body)
			if strings.EqualFold(ctype, "text/html") {
				m.HTMLBody += string(body)
			} else {
				m.TextBody += string(body)
			}
		case *gomail.AttachmentHeader:
			idx++
			ctype, _, _ := ph.ContentType()
			name, _ := ph.Filename()
			if name == "" {
				name = fmt.Sprintf("part-%d", idx)
			}
			// stream-count size without storing payload
			n, _ := io.Copy(io.Discard, p.Body)
			m.Attachments = append(m.Attachments, parsedAttachment{
				Filename:    name,
				ContentType: ctype,
				Size:        int(n),
				idx:         idx,
			})
		}
	}
	return m, nil
}

// readAttachment returns the (decoded) bytes of the attachment with the given
// 1-based index, or matching filename (case-insensitive). One of idx or name
// must be set.
func readAttachment(raw []byte, idx int, name string) (filename string, data []byte, err error) {
	r, err := gomail.CreateReader(bytes.NewReader(raw))
	if err != nil {
		return "", nil, fmt.Errorf("parse message: %w", err)
	}
	defer r.Close()

	current := 0
	for {
		p, err := r.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", nil, fmt.Errorf("next part: %w", err)
		}
		ah, ok := p.Header.(*gomail.AttachmentHeader)
		if !ok {
			io.Copy(io.Discard, p.Body)
			continue
		}
		current++
		fn, _ := ah.Filename()
		if fn == "" {
			fn = fmt.Sprintf("part-%d", current)
		}
		match := false
		if idx > 0 && current == idx {
			match = true
		}
		if name != "" && strings.EqualFold(fn, name) {
			match = true
		}
		if !match {
			io.Copy(io.Discard, p.Body)
			continue
		}
		b, err := io.ReadAll(p.Body)
		if err != nil {
			return "", nil, err
		}
		return fn, b, nil
	}
	if idx > 0 {
		return "", nil, fmt.Errorf("attachment index %d not found", idx)
	}
	return "", nil, fmt.Errorf("attachment %q not found", name)
}
