// File: dispatcher.email.smtp_mailpit_test.go

package grpop

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

const (
	testMailpitSMTPAddr = "localhost:1025"
	testMailpitAPIBase  = "http://localhost:8025"
)

// mailpitReachable reports whether a local Mailpit SMTP listener is up,
// matching the t.Skip-when-unreachable pattern every other real-backend
// test in this package already follows.
func mailpitReachable(t *testing.T) bool {
	t.Helper()
	conn, err := net.DialTimeout("tcp", testMailpitSMTPAddr, 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// mailpitMessageSummary is the subset of Mailpit's GET /api/v1/messages
// response fields this test needs.
type mailpitMessageSummary struct {
	ID      string
	From    struct{ Address string }
	To      []struct{ Address string }
	Subject string
	Snippet string
}

type mailpitListResponse struct {
	Messages []mailpitMessageSummary
}

type mailpitMessageDetail struct {
	Text string
	HTML string
}

// findMailpitMessageBySubject polls Mailpit's HTTP API for a message with
// the given subject, up to timeout — Send returning is not synchronous with
// Mailpit indexing the message for its API, so a short poll loop (not a
// single immediate GET) is needed to avoid a flaky false-negative.
func findMailpitMessageBySubject(t *testing.T, subject string, timeout time.Duration) mailpitMessageSummary {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(testMailpitAPIBase + "/api/v1/messages")
		if err == nil {
			var list mailpitListResponse
			if json.NewDecoder(resp.Body).Decode(&list) == nil {
				for _, m := range list.Messages {
					if m.Subject == subject {
						_ = resp.Body.Close()
						return m
					}
				}
			}
			_ = resp.Body.Close()
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no Mailpit message with subject %q found within %s", subject, timeout)
	return mailpitMessageSummary{}
}

func fetchMailpitMessageDetail(t *testing.T, id string) mailpitMessageDetail {
	t.Helper()
	resp, err := http.Get(testMailpitAPIBase + "/api/v1/message/" + id)
	if err != nil {
		t.Fatalf("fetch message detail: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read message detail body: %v", err)
	}
	var detail mailpitMessageDetail
	if err := json.Unmarshal(body, &detail); err != nil {
		t.Fatalf("decode message detail: %v", err)
	}
	return detail
}

func TestSMTPDispatcher_Mailpit_LiteralSend(t *testing.T) {
	if !mailpitReachable(t) {
		t.Skipf("Mailpit not available at %s, skipping", testMailpitSMTPAddr)
	}

	sender, err := NewSMTPDispatcher(SMTPDispatcherDeps{
		Addr:        testMailpitSMTPAddr,
		DefaultFrom: "grpop-test@example.com",
		TLSMode:     SMTPTLSInsecureNoTLS,
	})
	if err != nil {
		t.Fatalf("NewSMTPDispatcher: %v", err)
	}
	t.Cleanup(func() { _ = sender.Close() })

	subject := fmt.Sprintf("grpop literal test %d", time.Now().UnixNano())
	result, err := sender.Send(context.Background(), EmailMessage{
		To:       "recipient@example.com",
		Subject:  subject,
		HTMLBody: "<p>Hello from grpop</p>",
		TextBody: "Hello from grpop",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if result.Status != SendStatusSent {
		t.Fatalf("Status = %v, want SendStatusSent", result.Status)
	}

	msg := findMailpitMessageBySubject(t, subject, 5*time.Second)
	if msg.From.Address != "grpop-test@example.com" {
		t.Fatalf("From = %q, want grpop-test@example.com", msg.From.Address)
	}
	if len(msg.To) != 1 || msg.To[0].Address != "recipient@example.com" {
		t.Fatalf("To = %v, want [recipient@example.com]", msg.To)
	}

	detail := fetchMailpitMessageDetail(t, msg.ID)
	if detail.Text != "Hello from grpop" && !strings.Contains(detail.Text, "Hello from grpop") {
		t.Fatalf("Text = %q, want it to contain %q", detail.Text, "Hello from grpop")
	}
	if !strings.Contains(detail.HTML, "Hello from grpop") {
		t.Fatalf("HTML = %q, want it to contain %q", detail.HTML, "Hello from grpop")
	}
}

func TestSMTPDispatcher_Mailpit_TemplateRenderedSend(t *testing.T) {
	if !mailpitReachable(t) {
		t.Skipf("Mailpit not available at %s, skipping", testMailpitSMTPAddr)
	}

	engine := NewEmailTemplateEngine(EmailTemplateEngineConfig{})
	if err := engine.RegisterTemplate("mailpit-welcome", EmailTemplate{
		SubjectTemplate:  "Welcome {{.Name}}",
		HTMLBodyTemplate: "<p>Hi {{.Name}}, click <a href=\"{{.Link}}\">here</a></p>",
		TextBodyTemplate: "Hi {{.Name}}, visit {{.Link}}",
	}); err != nil {
		t.Fatalf("RegisterTemplate: %v", err)
	}

	sender, err := NewSMTPDispatcher(SMTPDispatcherDeps{
		Addr:           testMailpitSMTPAddr,
		DefaultFrom:    "grpop-test@example.com",
		TLSMode:        SMTPTLSInsecureNoTLS,
		TemplateEngine: engine,
	})
	if err != nil {
		t.Fatalf("NewSMTPDispatcher: %v", err)
	}
	t.Cleanup(func() { _ = sender.Close() })

	nonce := fmt.Sprintf("%d", time.Now().UnixNano())
	result, err := sender.Send(context.Background(), EmailMessage{
		To:           "invitee@example.com",
		TemplateName: "mailpit-welcome",
		TemplateData: map[string]any{"Name": "Grace-" + nonce, "Link": "https://example.com/" + nonce},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if result.Status != SendStatusSent {
		t.Fatalf("Status = %v, want SendStatusSent", result.Status)
	}

	subject := "Welcome Grace-" + nonce
	msg := findMailpitMessageBySubject(t, subject, 5*time.Second)
	detail := fetchMailpitMessageDetail(t, msg.ID)
	if !strings.Contains(detail.HTML, "https://example.com/"+nonce) {
		t.Fatalf("rendered HTML missing templated link: %s", detail.HTML)
	}
	if !strings.Contains(detail.Text, "https://example.com/"+nonce) {
		t.Fatalf("rendered text missing templated link: %s", detail.Text)
	}
}
