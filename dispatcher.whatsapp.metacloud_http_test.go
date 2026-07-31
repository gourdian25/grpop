// File: dispatcher.whatsapp.metacloud_http_test.go

package grpop

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestMetaCloudAPIClient points a real metaCloudAPIClient at a local
// httptest.Server instead of graph.facebook.com, by constructing it via
// newRealWhatsAppCloudAPIClient and then rewriting metaGraphAPIBaseURL is
// not possible (it's a package const) — so these tests exercise doJSON's
// request-building/response-parsing logic directly against a real HTTP
// server on a client whose base URL is swapped in via a small unexported
// test seam.
func newTestMetaCloudAPIClient(baseURL string) *metaCloudAPIClient {
	return &metaCloudAPIClient{
		httpClient:        &http.Client{Timeout: 5 * time.Second},
		phoneNumberID:     "test-phone-id",
		businessAccountID: "test-waba-id",
		accessToken:       "test-token",
		baseURL:           baseURL,
	}
}

func TestMetaCloudAPIClient_SendTemplateMessage_Success(t *testing.T) {
	var gotAuth, gotPath, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"messaging_product":"whatsapp","messages":[{"id":"wamid.HELLO"}]}`))
	}))
	defer server.Close()

	client := newTestMetaCloudAPIClient(server.URL)
	resp, err := client.SendTemplateMessage(context.Background(), WhatsAppCloudAPIRequest{
		To: "15551234567", TemplateName: "greeting", LanguageCode: "en_US",
		TemplateVariables: map[string]string{"1": "Ada"},
	})
	if err != nil {
		t.Fatalf("SendTemplateMessage: %v", err)
	}
	if resp.MessageID != "wamid.HELLO" {
		t.Fatalf("MessageID = %q, want wamid.HELLO", resp.MessageID)
	}
	if gotAuth != "Bearer test-token" {
		t.Fatalf("Authorization header = %q, want %q", gotAuth, "Bearer test-token")
	}
	if !strings.Contains(gotPath, "test-phone-id/messages") {
		t.Fatalf("request path = %q, want it to contain test-phone-id/messages", gotPath)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(gotBody), &decoded); err != nil {
		t.Fatalf("request body is not valid JSON: %v", err)
	}
	if decoded["to"] != "15551234567" {
		t.Fatalf("request body 'to' = %v, want 15551234567", decoded["to"])
	}
}

func TestMetaCloudAPIClient_SendTemplateMessage_VendorErrorEnvelope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"Invalid parameter","type":"OAuthException","code":100,"fbtrace_id":"abc"}}`))
	}))
	defer server.Close()

	client := newTestMetaCloudAPIClient(server.URL)
	_, err := client.SendTemplateMessage(context.Background(), WhatsAppCloudAPIRequest{To: "1", TemplateName: "t", LanguageCode: "en_US"})
	if err == nil {
		t.Fatal("SendTemplateMessage() err = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "Invalid parameter") {
		t.Fatalf("err = %v, want it to surface the Graph API error message", err)
	}
}

func TestMetaCloudAPIClient_GetApprovedTemplates_FiltersToApprovedOnly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "test-waba-id/message_templates") {
			t.Errorf("request path = %q, want it to contain test-waba-id/message_templates", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[
			{"name":"approved_one","language":"en_US","status":"APPROVED","components":[{"type":"BODY","text":"Hi {{1}}, order {{2}} shipped"}]},
			{"name":"pending_one","language":"en_US","status":"PENDING","components":[]},
			{"name":"no_params","language":"en_US","status":"APPROVED","components":[{"type":"BODY","text":"Hello there"}]}
		]}`))
	}))
	defer server.Close()

	client := newTestMetaCloudAPIClient(server.URL)
	templates, err := client.GetApprovedTemplates(context.Background())
	if err != nil {
		t.Fatalf("GetApprovedTemplates: %v", err)
	}
	if len(templates) != 2 {
		t.Fatalf("len(templates) = %d, want 2 (PENDING should be filtered out)", len(templates))
	}
	byName := make(map[string]WhatsAppTemplateInfo)
	for _, tpl := range templates {
		byName[tpl.Name] = tpl
	}
	if byName["approved_one"].ParameterCount != 2 {
		t.Fatalf("approved_one.ParameterCount = %d, want 2", byName["approved_one"].ParameterCount)
	}
	if byName["no_params"].ParameterCount != 0 {
		t.Fatalf("no_params.ParameterCount = %d, want 0", byName["no_params"].ParameterCount)
	}
}

func TestMetaCloudAPIClient_GetApprovedTemplates_RequiresBusinessAccountID(t *testing.T) {
	client := newTestMetaCloudAPIClient("http://unused.invalid")
	client.businessAccountID = ""
	if _, err := client.GetApprovedTemplates(context.Background()); err == nil {
		t.Fatal("GetApprovedTemplates() with empty BusinessAccountID err = nil, want non-nil")
	}
}

func TestMetaCloudAPIClient_SendTemplateMessage_MalformedSuccessBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{not valid json`))
	}))
	defer server.Close()

	client := newTestMetaCloudAPIClient(server.URL)
	_, err := client.SendTemplateMessage(context.Background(), WhatsAppCloudAPIRequest{To: "1", TemplateName: "t", LanguageCode: "en_US"})
	if err == nil {
		t.Fatal("SendTemplateMessage() with malformed JSON body err = nil, want non-nil")
	}
}

func TestMetaCloudAPIClient_SendTemplateMessage_NoMessageIDInResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"messaging_product":"whatsapp","messages":[]}`))
	}))
	defer server.Close()

	client := newTestMetaCloudAPIClient(server.URL)
	_, err := client.SendTemplateMessage(context.Background(), WhatsAppCloudAPIRequest{To: "1", TemplateName: "t", LanguageCode: "en_US"})
	if err == nil {
		t.Fatal("SendTemplateMessage() with an empty messages array err = nil, want non-nil")
	}
}

func TestMetaCloudAPIClient_GetApprovedTemplates_MalformedBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{not valid json`))
	}))
	defer server.Close()

	client := newTestMetaCloudAPIClient(server.URL)
	if _, err := client.GetApprovedTemplates(context.Background()); err == nil {
		t.Fatal("GetApprovedTemplates() with malformed JSON body err = nil, want non-nil")
	}
}

func TestMetaCloudAPIClient_DoJSON_NonJSONErrorBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`<html>gateway error</html>`))
	}))
	defer server.Close()

	client := newTestMetaCloudAPIClient(server.URL)
	_, err := client.SendTemplateMessage(context.Background(), WhatsAppCloudAPIRequest{To: "1", TemplateName: "t", LanguageCode: "en_US"})
	if err == nil {
		t.Fatal("SendTemplateMessage() err = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "status 500") {
		t.Fatalf("err = %v, want it to mention the raw status code since the body wasn't Meta's JSON error envelope", err)
	}
}
