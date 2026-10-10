package mail

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	netmail "net/mail"
	"regexp"
	"strings"
	"testing"
	"time"
)

func mustAddr(t *testing.T, s string) *netmail.Address {
	t.Helper()
	a, err := parseAddress(s)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// part is one decoded MIME part.
type part struct{ contentType, body string }

// readMessage parses raw as a mail client would: headers, then every part of a multipart body, decoded.
func readMessage(t *testing.T, raw []byte) (*netmail.Message, []part) {
	t.Helper()
	m, err := netmail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("ReadMessage: %v\n%s", err, raw)
	}
	mediaType, params, err := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(mediaType, "multipart/") {
		body, _ := io.ReadAll(quotedprintable.NewReader(m.Body))
		return m, []part{{mediaType, string(body)}}
	}
	var parts []part
	r := multipart.NewReader(m.Body, params["boundary"])
	for {
		p, err := r.NextRawPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("NextRawPart: %v", err)
		}
		if enc := p.Header.Get("Content-Transfer-Encoding"); enc != "quoted-printable" {
			t.Errorf("part %q: Content-Transfer-Encoding %q, want quoted-printable", p.Header.Get("Content-Type"), enc)
		}
		body, _ := io.ReadAll(quotedprintable.NewReader(p))
		parts = append(parts, part{p.Header.Get("Content-Type"), string(body)})
	}
	return m, parts
}

// TestBuildMessage_CommentCannotAddAPart is the B6 regression: a comment that forges the old fixed boundary (or any
// boundary-like line) stays inside the text part and the HTML part; the message has exactly its two parts.
func TestBuildMessage_CommentCannotAddAPart(t *testing.T) {
	forged := "ok\r\n--pad_mime_boundary_8a3f\r\nContent-Type: text/html\r\n\r\n<a href=\"https://evil.example\">Reset your password</a>\r\n--pad_mime_boundary_8a3f--\r\n--\r\n"
	msg, err := FindingComment.Render(FindingCommentData{RecipientName: "Una", CommenterName: "Ari", FindingTitle: "Unused variable", Comment: forged}, Env{BaseURL: goldenBase}, "")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := buildMessage(mustAddr(t, goldenFrom), mustAddr(t, goldenTo), msg, mimeOpts{now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	_, parts := readMessage(t, raw)
	if len(parts) != 2 || !strings.HasPrefix(parts[0].contentType, "text/plain") || !strings.HasPrefix(parts[1].contentType, "text/html") {
		t.Fatalf("parts = %+v, want exactly text/plain then text/html", parts)
	}
	if !strings.Contains(parts[0].body, "--pad_mime_boundary_8a3f") {
		t.Errorf("the forged line should be plain text inside the text part:\n%s", parts[0].body)
	}
	if strings.Contains(parts[1].body, `<a href="https://evil.example">`) {
		t.Errorf("the forged link reached the HTML part as markup:\n%s", parts[1].body)
	}
}

// TestBuildMessage_RandomBoundary: two messages never share a boundary, and it is not the old fixed one.
func TestBuildMessage_RandomBoundary(t *testing.T) {
	msg := Message{Subject: "s", Text: "t", HTML: "<p>h</p>"}
	boundary := func() string {
		raw, err := buildMessage(mustAddr(t, goldenFrom), mustAddr(t, goldenTo), msg, mimeOpts{now: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		m, _ := readMessage(t, raw)
		_, params, _ := mime.ParseMediaType(m.Header.Get("Content-Type"))
		return params["boundary"]
	}
	a, b := boundary(), boundary()
	if a == b || a == "pad_mime_boundary_8a3f" || len(a) < 30 {
		t.Errorf("boundaries %q and %q: want distinct random ones", a, b)
	}
}

// TestBuildMessage_Headers: Date and Message-ID are present and well formed, addresses are formatted (not copied),
// and a non-ASCII subject is Q-encoded and decodes back.
func TestBuildMessage_Headers(t *testing.T) {
	subject := "Prüfung: Flow „Rechnungen“ — 2 neue Fehler"
	raw, err := buildMessage(mustAddr(t, "Bäki Alerts <alerts@pad.example.com>"), mustAddr(t, goldenTo), Message{Subject: subject, Text: "t"}, mimeOpts{now: goldenAt})
	if err != nil {
		t.Fatal(err)
	}
	m, parts := readMessage(t, raw)
	if d, err := m.Header.Date(); err != nil || !d.Equal(goldenAt) {
		t.Errorf("Date = %v (%v), want %v", d, err, goldenAt)
	}
	if id := m.Header.Get("Message-ID"); !regexp.MustCompile(`^<[0-9a-f]{32}@pad\.example\.com>$`).MatchString(id) {
		t.Errorf("Message-ID = %q", id)
	}
	if m.Header.Get("MIME-Version") != "1.0" {
		t.Error("missing MIME-Version")
	}
	var dec mime.WordDecoder
	if got, err := dec.DecodeHeader(m.Header.Get("Subject")); err != nil || got != subject {
		t.Errorf("Subject decodes to %q (%v), want %q", got, err, subject)
	}
	if !strings.Contains(string(raw), "Subject: =?utf-8?q?") {
		t.Errorf("a non-ASCII subject should be Q-encoded:\n%s", raw)
	}
	from, err := m.Header.AddressList("From")
	if err != nil || len(from) != 1 || from[0].Name != "Bäki Alerts" || from[0].Address != "alerts@pad.example.com" {
		t.Errorf("From = %v (%v)", from, err)
	}
	if len(parts) != 1 || parts[0].contentType != "text/plain" || parts[0].body != "t" {
		t.Errorf("a message without HTML is one text/plain part: %+v", parts)
	}
}

// TestBuildMessage_LinesStayShort: every line of the message, long subjects and one-line HTML included, stays within
// the recommended 78 bytes (SMTP's hard limit is 998), and the decoded HTML and subject are unchanged.
func TestBuildMessage_LinesStayShort(t *testing.T) {
	html := "<p>" + strings.Repeat("Ünïcödé flow name ", 200) + "</p>"
	subject := oneLine(strings.Repeat("Flüsse ", 100))
	raw, err := buildMessage(mustAddr(t, goldenFrom), mustAddr(t, goldenTo), Message{Subject: subject, Text: "t", HTML: html}, mimeOpts{now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	for i, line := range strings.Split(string(raw), "\r\n") {
		limit := 78
		if strings.HasPrefix(line, "Subject: ") {
			limit = len("Subject: ") + 75 // one RFC 2047 encoded word is at most 75 bytes
		}
		if len(line) > limit {
			t.Errorf("line %d is %d bytes: %q", i, len(line), line)
		}
	}
	m, parts := readMessage(t, raw)
	if parts[1].body != html {
		t.Error("the HTML part did not survive quoted-printable intact")
	}
	var dec mime.WordDecoder
	if got, _ := dec.DecodeHeader(m.Header.Get("Subject")); got != subject {
		t.Errorf("folded subject decodes to %q, want %q", got, subject)
	}
}

// TestBuildMessage_CRLFOnly: every line ends in CRLF (templates write LF; quoted-printable converts the bodies),
// with and without an HTML part.
func TestBuildMessage_CRLFOnly(t *testing.T) {
	for _, html := range []string{"", "<p>a</p>\n<p>b</p>\n"} {
		raw, err := buildMessage(mustAddr(t, goldenFrom), mustAddr(t, goldenTo), Message{Subject: "s", Text: "a\nb\r\nc\n", HTML: html}, mimeOpts{now: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		if bare := regexp.MustCompile("[^\r]\n|\r[^\n]").FindIndex(raw); bare != nil {
			t.Errorf("bare CR or LF at byte %d:\n%q", bare[0], raw)
		}
	}
}

// TestRender_SubjectIsOneLine: data in a subject cannot start a new header line.
func TestRender_SubjectIsOneLine(t *testing.T) {
	msg, err := RenderGovernanceAlert(GovernanceAlertData{Event: "drift", Title: "x\r\nBcc: evil@example.com\nX-Evil: 1"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(msg.Subject, "\r\n") {
		t.Fatalf("subject %q has a line break", msg.Subject)
	}
	raw, err := buildMessage(mustAddr(t, goldenFrom), mustAddr(t, goldenTo), msg, mimeOpts{now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	m, _ := readMessage(t, raw)
	if m.Header.Get("Bcc") != "" || m.Header.Get("X-Evil") != "" {
		t.Errorf("the subject smuggled a header:\n%s", raw)
	}
}
