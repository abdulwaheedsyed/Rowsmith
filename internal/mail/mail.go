// Package mail sends email through an SMTP server: STARTTLS or implicit
// TLS, PLAIN or LOGIN sign-in, and MIME messages with text, HTML, embedded
// images and streamed attachments.
package mail

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Security modes.
const (
	StartTLS = "starttls" // plain connection upgraded with STARTTLS (port 587)
	TLS      = "tls"      // TLS from the start (port 465)
	None     = "none"     // no encryption: only for a relay on a trusted network
)

// Config is an SMTP server and the sender address.
type Config struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"-"`
	From     string `json:"from"` // "Reports <reports@example.com>" or an address
	Security string `json:"security"`
}

// Ready reports whether enough is set to try sending.
func (c Config) Ready() bool {
	return strings.TrimSpace(c.Host) != "" && strings.TrimSpace(c.From) != ""
}

func (c Config) port() int {
	if c.Port > 0 {
		return c.Port
	}
	switch c.Security {
	case TLS:
		return 465
	case None:
		return 25
	}
	return 587
}

// Attachment is streamed into the message when it is sent.
type Attachment struct {
	Name        string
	ContentType string
	Open        func() (io.ReadCloser, error)
}

// Inline is an image the HTML shows by its content ID (src="cid:…"), so it
// displays without loading anything from the network.
type Inline struct {
	CID         string
	ContentType string
	Data        []byte
}

type Message struct {
	To          []string
	Subject     string
	Text        string
	HTML        string
	Inline      []Inline
	Attachments []Attachment
}

//go:embed logo.png
var logoPNG []byte

// LogoCID is the content ID of the Rowsmith logo: HTML that shows
// src="cid:logo@rowsmith" gets the logo embedded in the message.
const LogoCID = "logo@rowsmith"

// Brand is the logo and wordmark as email HTML, for the top of a message.
const Brand = `<table role="presentation" cellpadding="0" cellspacing="0"><tr>` +
	`<td style="vertical-align:middle;"><img src="cid:` + LogoCID + `" width="28" height="28" alt="" style="display:block;border:0;"></td>` +
	`<td style="padding-left:9px;vertical-align:middle;font-size:17px;font-weight:800;letter-spacing:-.02em;color:#1B1F24;">rowsmith<sup style="font-size:9px;font-weight:500;letter-spacing:0;color:#7A828C;">™</sup></td>` +
	`</tr></table>`

// inlines lists the images to embed: m's own, plus the logo when the HTML
// shows it.
func (m Message) inlines() []Inline {
	out := m.Inline
	if strings.Contains(m.HTML, "cid:"+LogoCID) && !slices.ContainsFunc(out, func(x Inline) bool { return x.CID == LogoCID }) {
		out = append(slices.Clone(out), Inline{CID: LogoCID, ContentType: "image/png", Data: logoPNG})
	}
	return out
}

// Send delivers m through the server in c.
func Send(ctx context.Context, c Config, m Message) error {
	if !c.Ready() {
		return errors.New("email is not set up: an admin can add an SMTP server under Administration → Email")
	}
	from, err := mail.ParseAddress(c.From)
	if err != nil {
		return fmt.Errorf("the sender address %q is not valid", c.From)
	}
	if len(m.To) == 0 {
		return errors.New("no recipients")
	}
	for _, to := range m.To {
		if strings.ContainsAny(to, "\r\n<>,") {
			return fmt.Errorf("invalid recipient %q", to)
		}
	}
	host := strings.TrimSpace(c.Host)
	addr := net.JoinHostPort(host, strconv.Itoa(c.port()))
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(2 * time.Minute)
	}
	d := net.Dialer{Timeout: 20 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("could not reach the mail server %s: %v", addr, err)
	}
	_ = conn.SetDeadline(deadline)
	tlsConf := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	if c.Security == TLS {
		tc := tls.Client(conn, tlsConf)
		if err := tc.HandshakeContext(ctx); err != nil {
			conn.Close()
			return fmt.Errorf("TLS with %s failed: %v", addr, err)
		}
		conn = tc
	}
	cl, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("the mail server did not greet: %v", err)
	}
	defer cl.Close()
	if err := cl.Hello(helloName(from.Address)); err != nil {
		return smtpErr("greeting", err)
	}
	if c.Security == StartTLS || c.Security == "" {
		if ok, _ := cl.Extension("STARTTLS"); !ok {
			return errors.New("the mail server does not offer STARTTLS; choose TLS, or no encryption for a trusted relay")
		}
		if err := cl.StartTLS(tlsConf); err != nil {
			return fmt.Errorf("STARTTLS failed: %v", err)
		}
	}
	if c.Username != "" {
		_, encrypted := cl.TLSConnectionState()
		if !encrypted {
			return errors.New("signing in to the mail server needs an encrypted connection; choose STARTTLS or TLS")
		}
		ok, mechs := cl.Extension("AUTH")
		if !ok {
			return errors.New("the mail server does not accept sign-in; clear the username to send without it")
		}
		var auth smtp.Auth
		switch up := strings.ToUpper(mechs); {
		case strings.Contains(up, "PLAIN"):
			auth = smtp.PlainAuth("", c.Username, c.Password, host)
		case strings.Contains(up, "LOGIN"):
			auth = &loginAuth{user: c.Username, pass: c.Password}
		default:
			return fmt.Errorf("the mail server only offers sign-in methods Rowsmith does not support (%s)", mechs)
		}
		if err := cl.Auth(auth); err != nil {
			return smtpErr("sign-in", err)
		}
	}
	if err := cl.Mail(from.Address); err != nil {
		return smtpErr("sender", err)
	}
	for _, to := range m.To {
		if err := cl.Rcpt(to); err != nil {
			return smtpErr("recipient "+to, err)
		}
	}
	w, err := cl.Data()
	if err != nil {
		return smtpErr("message", err)
	}
	bw := bufio.NewWriterSize(w, 32<<10)
	if err := write(bw, from, m); err != nil {
		w.Close()
		return err
	}
	if err := bw.Flush(); err != nil {
		return smtpErr("message", err)
	}
	if err := w.Close(); err != nil {
		return smtpErr("message", err)
	}
	return cl.Quit()
}

func helloName(from string) string {
	if i := strings.LastIndexByte(from, '@'); i >= 0 && i < len(from)-1 {
		return from[i+1:]
	}
	return "localhost"
}

func smtpErr(step string, err error) error {
	var te *textproto.Error
	if errors.As(err, &te) {
		return fmt.Errorf("the mail server refused the %s: %d %s", step, te.Code, te.Msg)
	}
	return fmt.Errorf("the mail server failed at the %s: %v", step, err)
}

type loginAuth struct{ user, pass string }

func (a *loginAuth) Start(s *smtp.ServerInfo) (string, []byte, error) {
	if !s.TLS {
		return "", nil, errors.New("unencrypted connection")
	}
	return "LOGIN", nil, nil
}

func (a *loginAuth) Next(from []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	switch strings.ToLower(strings.TrimSpace(string(from))) {
	case "username:", "user name", "username":
		return []byte(a.user), nil
	case "password:", "password":
		return []byte(a.pass), nil
	}
	return nil, fmt.Errorf("unexpected sign-in prompt %q", from)
}

func header(v string) string { return strings.NewReplacer("\r", " ", "\n", " ").Replace(v) }

func write(w io.Writer, from *mail.Address, m Message) error {
	var id [12]byte
	_, _ = rand.Read(id[:])
	h := []string{
		"From: " + from.String(),
		"To: " + header(strings.Join(m.To, ", ")),
		"Subject: " + mime.QEncoding.Encode("utf-8", header(m.Subject)),
		"Date: " + time.Now().Format(time.RFC1123Z),
		"Message-ID: <" + hex.EncodeToString(id[:]) + "@" + helloName(from.Address) + ">",
		"MIME-Version: 1.0",
		"Auto-Submitted: auto-generated",
		"X-Auto-Response-Suppress: All",
	}
	mixed := multipart.NewWriter(w)
	alt := mixed
	top := "multipart/alternative"
	if len(m.Attachments) > 0 {
		top = "multipart/mixed"
	}
	h = append(h, fmt.Sprintf("Content-Type: %s; boundary=%q", top, mixed.Boundary()))
	if _, err := io.WriteString(w, strings.Join(h, "\r\n")+"\r\n\r\n"); err != nil {
		return err
	}
	if len(m.Attachments) > 0 {
		var err error
		if alt, err = nested(mixed, "multipart/alternative"); err != nil {
			return err
		}
	}
	if err := writeBodies(alt, m); err != nil {
		return err
	}
	if alt != mixed {
		if err := alt.Close(); err != nil {
			return err
		}
	}
	for _, a := range m.Attachments {
		name := header(a.Name)
		part, err := mixed.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {mime.FormatMediaType(strOr(a.ContentType, "application/octet-stream"), map[string]string{"name": name})},
			"Content-Disposition":       {fmt.Sprintf("attachment; filename*=UTF-8''%s", url.PathEscape(name))},
			"Content-Transfer-Encoding": {"base64"},
		})
		if err != nil {
			return err
		}
		rc, err := a.Open()
		if err != nil {
			return fmt.Errorf("could not read the attachment %s: %v", name, err)
		}
		lw := &lineWriter{w: part}
		enc := base64.NewEncoder(base64.StdEncoding, lw)
		_, err = io.Copy(enc, rc)
		rc.Close()
		if err == nil {
			err = enc.Close()
		}
		if err == nil {
			err = lw.end()
		}
		if err != nil {
			return fmt.Errorf("could not attach %s: %v", name, err)
		}
	}
	return mixed.Close()
}

// writeBodies writes the text and HTML versions. With embedded images the
// HTML goes in a multipart/related part alongside them.
func writeBodies(alt *multipart.Writer, m Message) error {
	if m.Text != "" {
		if err := writeQP(alt, "text/plain", m.Text); err != nil {
			return err
		}
	}
	if m.HTML == "" {
		return nil
	}
	images := m.inlines()
	if len(images) == 0 {
		return writeQP(alt, "text/html", m.HTML)
	}
	rel, err := nested(alt, `multipart/related; type="text/html"`)
	if err != nil {
		return err
	}
	if err := writeQP(rel, "text/html", m.HTML); err != nil {
		return err
	}
	for _, img := range images {
		part, err := rel.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {img.ContentType},
			"Content-ID":                {"<" + img.CID + ">"},
			"Content-Disposition":       {"inline"},
			"Content-Transfer-Encoding": {"base64"},
		})
		if err != nil {
			return err
		}
		lw := &lineWriter{w: part}
		enc := base64.NewEncoder(base64.StdEncoding, lw)
		if _, err := io.Copy(enc, bytes.NewReader(img.Data)); err != nil {
			return err
		}
		if err := enc.Close(); err != nil {
			return err
		}
		if err := lw.end(); err != nil {
			return err
		}
	}
	return rel.Close()
}

// nested opens a multipart part of the given type inside parent.
func nested(parent *multipart.Writer, typ string) (*multipart.Writer, error) {
	boundary := multipart.NewWriter(nil).Boundary()
	part, err := parent.CreatePart(textproto.MIMEHeader{"Content-Type": {fmt.Sprintf("%s; boundary=%q", typ, boundary)}})
	if err != nil {
		return nil, err
	}
	w := multipart.NewWriter(part)
	return w, w.SetBoundary(boundary)
}

func writeQP(w *multipart.Writer, typ, text string) error {
	part, err := w.CreatePart(textproto.MIMEHeader{
		"Content-Type":              {typ + "; charset=utf-8"},
		"Content-Transfer-Encoding": {"quoted-printable"},
	})
	if err != nil {
		return err
	}
	qp := quotedprintable.NewWriter(part)
	if _, err := io.WriteString(qp, text); err != nil {
		return err
	}
	return qp.Close()
}

// lineWriter breaks base64 into 76-character lines.
type lineWriter struct {
	w   io.Writer
	col int
}

func (l *lineWriter) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 {
		k := min(76-l.col, len(p))
		if _, err := l.w.Write(p[:k]); err != nil {
			return n, err
		}
		n += k
		l.col += k
		p = p[k:]
		if l.col == 76 {
			if _, err := io.WriteString(l.w, "\r\n"); err != nil {
				return n, err
			}
			l.col = 0
		}
	}
	return n, nil
}

func (l *lineWriter) end() error {
	if l.col > 0 {
		_, err := io.WriteString(l.w, "\r\n")
		return err
	}
	return nil
}

func strOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
