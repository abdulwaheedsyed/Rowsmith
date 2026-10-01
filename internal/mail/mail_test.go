package mail

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/mail"
	"strconv"
	"strings"
	"testing"
)

// fakeServer accepts one message without TLS and returns what it received.
func fakeServer(t *testing.T, ext string) (int, chan string) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan string, 1)
	go func() {
		defer ln.Close()
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		say := func(s string) { io.WriteString(c, s+"\r\n") }
		say("220 fake ESMTP")
		var data strings.Builder
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				got <- data.String()
				return
			}
			cmd := strings.ToUpper(strings.TrimSpace(line))
			switch {
			case strings.HasPrefix(cmd, "EHLO"):
				say("250-fake")
				if ext != "" {
					say("250-" + ext)
				}
				say("250 8BITMIME")
			case strings.HasPrefix(cmd, "MAIL"), strings.HasPrefix(cmd, "RCPT"):
				say("250 ok")
			case cmd == "DATA":
				say("354 go")
				for {
					l, _ := r.ReadString('\n')
					if l == ".\r\n" {
						break
					}
					data.WriteString(l)
				}
				say("250 queued")
			case cmd == "QUIT":
				say("221 bye")
				got <- data.String()
				return
			default:
				say("502 no")
			}
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, got
}

func TestSendBuildsAValidMessage(t *testing.T) {
	port, got := fakeServer(t, "")
	file := bytes.Repeat([]byte("sku,stock\nA-1,4\n"), 1000)
	err := Send(context.Background(), Config{Host: "127.0.0.1", Port: port, From: "Rowsmith Reports <reports@example.com>", Security: None}, Message{
		To:      []string{"ops@example.com", "ana@example.org"},
		Subject: "Low stock: 3 rows — café",
		Text:    "Plain text body",
		HTML:    "<p>HTML body with a long line " + strings.Repeat("x", 300) + "</p>",
		Attachments: []Attachment{{Name: "low stock.csv", ContentType: "text/csv", Open: func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(file)), nil
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw := <-got
	msg, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	dec := new(mime.WordDecoder)
	subj, _ := dec.DecodeHeader(msg.Header.Get("Subject"))
	if subj != "Low stock: 3 rows — café" || msg.Header.Get("Auto-Submitted") != "auto-generated" {
		t.Fatalf("headers: %q %v", subj, msg.Header)
	}
	_, params, _ := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	mr := multipart.NewReader(msg.Body, params["boundary"])
	alt, _ := mr.NextPart()
	_, ap, _ := mime.ParseMediaType(alt.Header.Get("Content-Type"))
	ar := multipart.NewReader(alt, ap["boundary"])
	var types []string
	for {
		p, err := ar.NextPart()
		if err != nil {
			break
		}
		b, _ := io.ReadAll(p) // multipart decodes quoted-printable
		types = append(types, p.Header.Get("Content-Type")+"="+strconv.Itoa(len(b)))
	}
	if len(types) != 2 || !strings.HasPrefix(types[0], "text/plain") || !strings.HasPrefix(types[1], "text/html") {
		t.Fatalf("alternative parts: %v", types)
	}
	att, err := mr.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	if att.FileName() != "low stock.csv" {
		t.Fatalf("attachment name %q", att.FileName())
	}
	enc, _ := io.ReadAll(att)
	for _, line := range strings.Split(strings.TrimSpace(string(enc)), "\r\n") {
		if len(line) > 76 {
			t.Fatalf("base64 line of %d characters", len(line))
		}
	}
	body, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(string(enc), "\r\n", ""))
	if err != nil || !bytes.Equal(body, file) {
		t.Fatalf("attachment differs: %v", err)
	}
}

func TestSendRefusesUnsafeSetups(t *testing.T) {
	port, _ := fakeServer(t, "AUTH PLAIN")
	err := Send(context.Background(), Config{Host: "127.0.0.1", Port: port, From: "r@example.com", Security: None, Username: "u", Password: "p"},
		Message{To: []string{"a@example.com"}, Subject: "x", Text: "x"})
	if err == nil || !strings.Contains(err.Error(), "encrypted") {
		t.Fatalf("sign-in over plain text should be refused: %v", err)
	}
	port, _ = fakeServer(t, "")
	err = Send(context.Background(), Config{Host: "127.0.0.1", Port: port, From: "r@example.com", Security: StartTLS},
		Message{To: []string{"a@example.com"}, Subject: "x", Text: "x"})
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("missing STARTTLS should be reported: %v", err)
	}
	err = Send(context.Background(), Config{Host: "127.0.0.1", Port: 1, From: "r@example.com", Security: None},
		Message{To: []string{"a@example.com\r\nBcc: x@y.z"}, Subject: "x", Text: "x"})
	if err == nil || !strings.Contains(err.Error(), "invalid recipient") {
		t.Fatalf("header injection should be refused: %v", err)
	}
}

// TestLogoIsEmbedded checks that HTML showing the logo carries it as a
// related part, next to the HTML and inside the alternative with the text.
func TestLogoIsEmbedded(t *testing.T) {
	var buf bytes.Buffer
	m := Message{To: []string{"a@example.com"}, Subject: "s", Text: "plain", HTML: "<p>" + Brand + "</p>",
		Attachments: []Attachment{{Name: "r.csv", ContentType: "text/csv", Open: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("a,b")), nil }}}}
	if err := write(&buf, &mail.Address{Address: "from@example.com"}, m); err != nil {
		t.Fatal(err)
	}
	msg, err := mail.ReadMessage(&buf)
	if err != nil {
		t.Fatal(err)
	}
	var tree []string
	var walk func(r io.Reader, ctype, indent string)
	walk = func(r io.Reader, ctype, indent string) {
		mt, params, _ := mime.ParseMediaType(ctype)
		tree = append(tree, indent+mt)
		if !strings.HasPrefix(mt, "multipart/") {
			return
		}
		mr := multipart.NewReader(r, params["boundary"])
		for {
			p, err := mr.NextPart()
			if err != nil {
				return
			}
			if cid := p.Header.Get("Content-ID"); cid != "" {
				data, _ := io.ReadAll(base64.NewDecoder(base64.StdEncoding, p))
				if cid != "<"+LogoCID+">" || !bytes.Equal(data, logoPNG) {
					t.Errorf("inline part %s: %d bytes", cid, len(data))
				}
			}
			walk(p, p.Header.Get("Content-Type"), indent+"  ")
		}
	}
	walk(msg.Body, msg.Header.Get("Content-Type"), "")
	want := []string{"multipart/mixed", "  multipart/alternative", "    text/plain", "    multipart/related", "      text/html", "      image/png", "  text/csv"}
	if strings.Join(tree, "\n") != strings.Join(want, "\n") {
		t.Fatalf("structure:\n%s", strings.Join(tree, "\n"))
	}
}
