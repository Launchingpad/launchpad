package login

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"devops/internal/login/smtptest"
)

// LAUN-34: Message / MessageMailer (decisions-T13 #26).

func testMessage() Message {
	return Message{
		To: "chen@example.com", Subject: "[LAUN-1] Assigned to you: A title",
		Text: "plain body\n", HTML: "<p>html body</p>\n",
		Headers: map[string]string{
			"List-Unsubscribe":      "<https://lp.example.com/api/v1/notifications/unsubscribe/tok>",
			"List-Unsubscribe-Post": "List-Unsubscribe=One-Click",
			"Message-ID":            "<0190aaaa-0000-7000-8000-000000000001@lp.example.com>",
		},
	}
}

func TestPostmarkMailer_SendMessage(t *testing.T) {
	var got struct {
		Text, HTML string
		Headers    []struct{ Name, Value string }
		To         string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var raw map[string]json.RawMessage
		_ = json.Unmarshal(b, &raw)
		_ = json.Unmarshal(raw["TextBody"], &got.Text)
		_ = json.Unmarshal(raw["HtmlBody"], &got.HTML)
		_ = json.Unmarshal(raw["Headers"], &got.Headers)
		_ = json.Unmarshal(raw["To"], &got.To)
		w.WriteHeader(200)
	}))
	defer srv.Close()
	m := NewPostmarkMailer("server-token", "tracker@example.com", "")
	m.Endpoint = srv.URL
	if err := m.SendMessage(context.Background(), testMessage()); err != nil {
		t.Fatal(err)
	}
	if got.Text != "plain body\n" || got.HTML != "<p>html body</p>\n" || got.To != "chen@example.com" {
		t.Fatalf("bodies %+v", got)
	}
	want := []string{"List-Unsubscribe", "List-Unsubscribe-Post", "Message-ID"} // sorted
	if len(got.Headers) != 3 {
		t.Fatalf("headers %+v", got.Headers)
	}
	for i, h := range got.Headers {
		if h.Name != want[i] || h.Value != testMessage().Headers[want[i]] {
			t.Fatalf("header %d = %+v", i, h)
		}
	}
	// A header that could inject another is refused before any request.
	bad := testMessage()
	bad.Headers = map[string]string{"X-A": "v\r\nBcc: victim@example.com"}
	if err := m.SendMessage(context.Background(), bad); err == nil {
		t.Fatal("CRLF in a header value was sent")
	}
}

func TestSMTPMailer_SendMessage_MultipartAndHeaders(t *testing.T) {
	srv := smtptest.Start(t, smtptest.Options{})
	m := smtpMailer(srv, "mail.test", SMTPNone)
	if err := m.SendMessage(context.Background(), testMessage()); err != nil {
		t.Fatal(err)
	}
	msgs := srv.Messages()
	if len(msgs) != 1 {
		t.Fatalf("messages %d", len(msgs))
	}
	pm, err := msgs[0].Parse()
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range testMessage().Headers {
		if got := pm.Header.Get(k); got != want {
			t.Errorf("header %s = %q, want %q", k, got, want)
		}
	}
	if n := len(pm.Header["Message-Id"]); n != 1 {
		t.Fatalf("Message-ID headers = %d, want the custom one only", n)
	}
	mt, params, err := mime.ParseMediaType(pm.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/alternative" {
		t.Fatalf("content type %q: %v", pm.Header.Get("Content-Type"), err)
	}
	mr := multipart.NewReader(pm.Body, params["boundary"])
	var types, bodies []string
	for {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(quotedprintable.NewReader(p))
		types = append(types, p.Header.Get("Content-Type"))
		bodies = append(bodies, string(b))
	}
	if len(types) != 2 || !strings.HasPrefix(types[0], "text/plain") || !strings.HasPrefix(types[1], "text/html") {
		t.Fatalf("parts %v", types)
	}
	if !strings.Contains(bodies[0], "plain body") || !strings.Contains(bodies[1], "<p>html body</p>") {
		t.Fatalf("bodies %q", bodies)
	}
}

func TestSMTPMailer_SendMessage_PlainStillSinglePart(t *testing.T) {
	srv := smtptest.Start(t, smtptest.Options{})
	m := smtpMailer(srv, "mail.test", SMTPNone)
	if err := m.SendText(context.Background(), "chen@example.com", "s", "t"); err != nil {
		t.Fatal(err)
	}
	pm, err := srv.Messages()[0].Parse()
	if err != nil {
		t.Fatal(err)
	}
	if pm.Header.Get("Content-Type") != "text/plain; charset=UTF-8" {
		t.Fatalf("content type %q", pm.Header.Get("Content-Type"))
	}
}

func TestMessageHeaders_Refused(t *testing.T) {
	now := time.Now()
	for name, h := range map[string]map[string]string{
		"CR in value":        {"X-A": "a\rb"},
		"LF in value":        {"X-A": "a\nBcc: v@example.com"},
		"NUL in value":       {"X-A": "a\x00b"},
		"non-ASCII value":    {"X-A": "Grüße"},
		"space in name":      {"X A": "planted-9"},
		"colon in name":      {"X-A: B": "planted-9"},
		"empty name":         {"": "planted-9"},
		"reserved To":        {"To": "x@example.com"},
		"reserved Subject":   {"subject": "x"},
		"reserved Bcc":       {"Bcc": "x@example.com"},
		"duplicate by case":  {"X-A": "1", "x-a": "2"},
		"bare Message-ID":    {"Message-ID": "no-brackets"},
		"empty Message-ID":   {"Message-ID": "<>"},
		"content type reset": {"Content-Type": "text/html"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := buildFullMessage("tracker@example.com", "", Message{To: "chen@example.com", Subject: "s", Text: "t", Headers: h}, now)
			var me *MailError
			if !errors.As(err, &me) {
				t.Fatalf("err = %v, want a *MailError", err)
			}
			for _, v := range h {
				if v != "" && strings.Contains(me.Msg, v) {
					t.Fatalf("the error repeats a header value: %q", me.Msg)
				}
			}
		})
	}
}

func TestDevLogMailer_SendMessage_SubjectOnly(t *testing.T) {
	var buf bytes.Buffer
	m, err := NewDevLogMailer(slog.New(slog.NewTextHandler(&buf, nil)), true)
	if err != nil {
		t.Fatal(err)
	}
	msg := testMessage()
	msg.Headers["List-Unsubscribe"] = "<https://x.example/tok-planted-4411>"
	if err := m.SendMessage(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "Assigned to you") {
		t.Fatalf("the subject is not logged: %s", out)
	}
	for _, bad := range []string{"chen@example.com", "plain body", "html body", "tok-planted-4411"} {
		if strings.Contains(out, bad) {
			t.Fatalf("the dev log holds %q: %s", bad, out)
		}
	}
}

// textOnly is a TextMailer that is not a MessageMailer.
type textOnly struct{ countMailer }

func TestMailSwitch_SendMessage(t *testing.T) {
	s := NewMailSwitch()
	if err := s.SendMessage(context.Background(), testMessage()); !errors.Is(err, ErrNoMailer) {
		t.Fatalf("no transport: %v", err)
	}
	// A TextMailer that cannot send a Message is an error, not a silent drop
	// of the headers.
	s.Set(&textOnly{})
	var me *MailError
	if err := s.SendMessage(context.Background(), testMessage()); !errors.As(err, &me) {
		t.Fatalf("text-only transport: %v", err)
	}
	srv := smtptest.Start(t, smtptest.Options{})
	s.Set(smtpMailer(srv, "mail.test", SMTPNone))
	if err := s.SendMessage(context.Background(), testMessage()); err != nil {
		t.Fatal(err)
	}
	if len(srv.Messages()) != 1 {
		t.Fatal("the switch did not forward the message")
	}
}
