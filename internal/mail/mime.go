package mail

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	netmail "net/mail"
	"net/textproto"
	"strings"
	"time"
)

// mimeOpts fixes what a message would otherwise draw from the clock and crypto/rand; only goldens set more than now.
type mimeOpts struct {
	now       time.Time
	boundary  string // "" draws a random boundary (mime/multipart)
	messageID string // "" draws a random id
}

// buildMessage writes msg as an RFC 5322 message: From/To from parsed addresses (RFC 2047 display names), a
// Q-encoded Subject, Date, Message-ID, and a body that is text/plain alone or multipart/alternative with an
// unguessable boundary, every part quoted-printable (CRLF lines, none over 76 bytes, 7-bit safe). User text in a
// part cannot end it: it would have to contain the boundary, which is drawn per message after the text is rendered.
func buildMessage(from, to *netmail.Address, msg Message, o mimeOpts) ([]byte, error) {
	id := o.messageID
	if id == "" {
		var err error
		if id, err = newMessageID(from.Address); err != nil {
			return nil, err
		}
	}
	var b bytes.Buffer
	header := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	header("From", from.String())
	header("To", to.String())
	header("Subject", encodeSubject(msg.Subject))
	header("Date", o.now.Format(time.RFC1123Z))
	header("Message-ID", id)
	header("MIME-Version", "1.0")
	if msg.HTML == "" {
		header("Content-Type", "text/plain; charset=UTF-8")
		header("Content-Transfer-Encoding", "quoted-printable")
		b.WriteString("\r\n")
		if err := writeQP(&b, msg.Text); err != nil {
			return nil, err
		}
		return b.Bytes(), nil
	}
	mw := multipart.NewWriter(&b)
	if o.boundary != "" {
		if err := mw.SetBoundary(o.boundary); err != nil {
			return nil, err
		}
	}
	// Folded before the parameter: the 60-character boundary would push the line past the recommended 78.
	contentType := mime.FormatMediaType("multipart/alternative", map[string]string{"boundary": mw.Boundary()})
	header("Content-Type", strings.Replace(contentType, "; ", ";\r\n ", 1))
	b.WriteString("\r\n")
	for _, part := range []struct{ contentType, body string }{
		{"text/plain; charset=UTF-8", msg.Text},
		{"text/html; charset=UTF-8", msg.HTML},
	} {
		w, err := mw.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {part.contentType},
			"Content-Transfer-Encoding": {"quoted-printable"},
		})
		if err != nil {
			return nil, err
		}
		if err := writeQP(w, part.body); err != nil {
			return nil, err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func writeQP(w io.Writer, body string) error {
	qp := quotedprintable.NewWriter(w)
	if _, err := io.WriteString(qp, body); err != nil {
		return err
	}
	return qp.Close()
}

// encodeSubject Q-encodes a subject that is not plain printable ASCII and folds the encoded words onto
// continuation lines, so no header line nears the 998-byte limit.
func encodeSubject(s string) string {
	enc := mime.QEncoding.Encode("utf-8", s)
	if enc == s {
		return s
	}
	return strings.ReplaceAll(enc, "?= =?", "?=\r\n =?")
}

// newMessageID returns "<random@domain>" with the sender's domain.
func newMessageID(fromAddr string) (string, error) {
	var r [16]byte
	if _, err := rand.Read(r[:]); err != nil {
		return "", fmt.Errorf("mail: message id: %w", err)
	}
	domain := "localhost"
	if at := strings.LastIndexByte(fromAddr, '@'); at >= 0 && at < len(fromAddr)-1 {
		domain = fromAddr[at+1:]
	}
	return "<" + hex.EncodeToString(r[:]) + "@" + domain + ">", nil
}

// parseAddress parses one address (a bare one or "Name <addr>"); CR, LF and lists are refused, never stripped.
func parseAddress(s string) (*netmail.Address, error) {
	if strings.ContainsAny(s, "\r\n") {
		return nil, fmt.Errorf("mail: address contains a line break")
	}
	a, err := netmail.ParseAddress(s)
	if err != nil {
		return nil, fmt.Errorf("mail: invalid address: %w", err)
	}
	return a, nil
}
