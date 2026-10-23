// File: dispatcher.whatsapp.metacloud_http_test.go

package grpop

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
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
		apiVersion:        defaultMetaGraphAPIVersion,
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
		Body: []WhatsAppParameter{WhatsAppText("Ada")},
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

// TestMetaCloudAPIClient_ClassifiesRefusals: Meta's retry-later codes, a 429
// and a 5xx are temporary; every other refusal is permanent (Meta's error
// reference, read 2026-10-23).
func TestMetaCloudAPIClient_ClassifiesRefusals(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		permanent bool
	}{
		{"100 invalid parameter", 400, `{"error":{"message":"(#100) Invalid parameter","code":100}}`, true},
		{"131026 undeliverable", 400, `{"error":{"message":"Message undeliverable","code":131026}}`, true},
		{"132001 template does not exist", 404, `{"error":{"message":"Template name does not exist","code":132001}}`, true},
		{"190 expired token", 401, `{"error":{"message":"Session has expired","code":190}}`, true},
		{"a 404 with no envelope", 404, `not json`, true},
		{"130429 throughput", 400, `{"error":{"message":"Rate limit hit","code":130429}}`, false},
		{"131056 pair rate limit", 400, `{"error":{"message":"Pair rate limit","code":131056}}`, false},
		{"4 app rate limit", 400, `{"error":{"message":"Application request limit reached","code":4}}`, false},
		{"131000 unknown", 500, `{"error":{"message":"Something went wrong","code":131000}}`, false},
		{"a 429 with no envelope", 429, ``, false},
		{"a 503 with no envelope", 503, ``, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			_, err := newTestMetaCloudAPIClient(server.URL).SendTemplateMessage(context.Background(), WhatsAppCloudAPIRequest{To: "+15551234567", TemplateName: "t", LanguageCode: "en_US"})
			if err == nil {
				t.Fatal("SendTemplateMessage() err = nil, want a refusal")
			}
			if got := errors.Is(err, ErrPermanent); got != tc.permanent {
				t.Fatalf("errors.Is(%v, ErrPermanent) = %v, want %v", err, got, tc.permanent)
			}
		})
	}

	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := server.URL
	server.Close()
	_, err := newTestMetaCloudAPIClient(url).SendTemplateMessage(context.Background(), WhatsAppCloudAPIRequest{To: "+15551234567", TemplateName: "t", LanguageCode: "en_US"})
	if err == nil || errors.Is(err, ErrPermanent) {
		t.Fatalf("a network failure = %v, want a temporary failure", err)
	}
}

// TestNewMetaCloudWhatsAppDispatcher_GraphAPIVersion: every call names the
// configured version, the current default when none is set; a malformed one
// fails at construction.
func TestNewMetaCloudWhatsAppDispatcher_GraphAPIVersion(t *testing.T) {
	for _, tc := range []struct{ configured, want string }{{"", defaultMetaGraphAPIVersion}, {"v27.0", "v27.0"}} {
		var gotPath string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			_, _ = w.Write([]byte(`{"messages":[{"id":"wamid.1"}]}`))
		}))
		sender, err := NewMetaCloudWhatsAppDispatcher(MetaCloudDispatcherDeps{PhoneNumberID: "pid", AccessToken: "tok", GraphAPIVersion: tc.configured})
		if err != nil {
			t.Fatalf("NewMetaCloudWhatsAppDispatcher(%q): %v", tc.configured, err)
		}
		sender.(*metaCloudDispatcher).client.(*metaCloudAPIClient).baseURL = server.URL
		if _, err := sender.Send(context.Background(), validWhatsAppMessage()); err != nil {
			t.Fatalf("Send: %v", err)
		}
		server.Close()
		if gotPath != "/"+tc.want+"/pid/messages" {
			t.Fatalf("GraphAPIVersion %q called %q, want /%s/pid/messages", tc.configured, gotPath, tc.want)
		}
	}
	if defaultMetaGraphAPIVersion == "v20.0" {
		t.Fatal("the default is v20.0, which Meta stopped serving on 2026-09-24")
	}
	for _, bad := range []string{"26.0", "v26", "latest", "v26.0/../x"} {
		if _, err := NewMetaCloudWhatsAppDispatcher(MetaCloudDispatcherDeps{PhoneNumberID: "pid", AccessToken: "tok", GraphAPIVersion: bad}); err == nil {
			t.Errorf("GraphAPIVersion %q was accepted", bad)
		}
	}
}

// sentTemplate sends msg through a real Meta sender pointed at a local
// server and returns the request's "template" object, after checking the
// envelope around it.
func sentTemplate(t *testing.T, msg WhatsAppMessage) any {
	t.Helper()
	var raw []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"messages":[{"id":"wamid.1"}]}`))
	}))
	defer server.Close()
	sender, err := NewMetaCloudWhatsAppDispatcher(MetaCloudDispatcherDeps{PhoneNumberID: "pid", AccessToken: "tok"})
	if err != nil {
		t.Fatalf("NewMetaCloudWhatsAppDispatcher: %v", err)
	}
	sender.(*metaCloudDispatcher).client.(*metaCloudAPIClient).baseURL = server.URL
	if _, err := sender.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	var request struct {
		MessagingProduct string `json:"messaging_product"`
		To               string `json:"to"`
		Type             string `json:"type"`
		Template         any    `json:"template"`
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		t.Fatalf("the request isn't JSON: %v: %s", err, raw)
	}
	if request.MessagingProduct != "whatsapp" || request.To != msg.To || request.Type != "template" {
		t.Fatalf("the envelope = %s", raw)
	}
	return request.Template
}

// TestMetaCloudDispatcher_Send_Components: every value kind reaches Meta in
// the exact shape Meta's send API takes (its template components,
// authentication and coupon template pages, read 2026-10-24): typed
// parameters, a named template's parameter_name, a button's sub_type and
// string index, and a one-time code twice.
func TestMetaCloudDispatcher_Send_Components(t *testing.T) {
	otpBody, otpButtons := WhatsAppOTP("123456")
	imageByID := WhatsAppParameter{Media: &WhatsAppMedia{Kind: WhatsAppMediaImage, ID: "1234"}}
	video := WhatsAppVideo("https://example.com/v.mp4")
	document := WhatsAppDocument("https://example.com/invoice.pdf", "invoice.pdf")
	cases := []struct {
		name string
		msg  WhatsAppMessage
		want string
	}{
		{"nothing to fill", WhatsAppMessage{TemplateName: "hello_world"},
			`{"name":"hello_world","language":{"code":"en_US"}}`},
		{"positional: a text header, every body kind, every button kind", WhatsAppMessage{
			TemplateName: "order_update",
			Header:       &WhatsAppParameter{Text: "Order 42"},
			Body: []WhatsAppParameter{
				WhatsAppText("Ada"),
				{Currency: &WhatsAppCurrency{FallbackValue: "$100.99", Code: "USD", Amount1000: 100990}},
				{DateTime: &WhatsAppDateTime{FallbackValue: "Feb 25, 1977"}},
			},
			Buttons: []WhatsAppButton{
				{Index: 0, Kind: WhatsAppButtonURL, Value: "track/42"},
				{Index: 1, Kind: WhatsAppButtonQuickReply, Value: "stop-promos"},
				{Index: 2, Kind: WhatsAppButtonCopyCode, Value: "SAVE20"},
			},
		}, `{"name":"order_update","language":{"code":"en_US"},"components":[
			{"type":"header","parameters":[{"type":"text","text":"Order 42"}]},
			{"type":"body","parameters":[
				{"type":"text","text":"Ada"},
				{"type":"currency","currency":{"fallback_value":"$100.99","code":"USD","amount_1000":100990}},
				{"type":"date_time","date_time":{"fallback_value":"Feb 25, 1977"}}]},
			{"type":"button","sub_type":"url","index":"0","parameters":[{"type":"text","text":"track/42"}]},
			{"type":"button","sub_type":"quick_reply","index":"1","parameters":[{"type":"payload","payload":"stop-promos"}]},
			{"type":"button","sub_type":"copy_code","index":"2","parameters":[{"type":"coupon_code","coupon_code":"SAVE20"}]}]}`},
		{"named", WhatsAppMessage{
			TemplateName: "named",
			Header:       &WhatsAppParameter{Name: "order_id", Text: "42"},
			Body: []WhatsAppParameter{
				WhatsAppNamedText("first_name", "Ada"),
				{Name: "total", Currency: &WhatsAppCurrency{FallbackValue: "$1", Code: "EUR", Amount1000: 1000}},
			},
			Buttons: []WhatsAppButton{{Index: 0, Kind: WhatsAppButtonURL, Value: "Gon%C3%A7alves", Name: "customer_name"}},
		}, `{"name":"named","language":{"code":"en_US"},"components":[
			{"type":"header","parameters":[{"type":"text","parameter_name":"order_id","text":"42"}]},
			{"type":"body","parameters":[
				{"type":"text","parameter_name":"first_name","text":"Ada"},
				{"type":"currency","parameter_name":"total","currency":{"fallback_value":"$1","code":"EUR","amount_1000":1000}}]},
			{"type":"button","sub_type":"url","index":"0","parameters":[{"type":"text","parameter_name":"customer_name","text":"Gon%C3%A7alves"}]}]}`},
		{"a one-time code", WhatsAppMessage{TemplateName: "login_code", Body: otpBody, Buttons: otpButtons},
			`{"name":"login_code","language":{"code":"en_US"},"components":[
			{"type":"body","parameters":[{"type":"text","text":"123456"}]},
			{"type":"button","sub_type":"url","index":"0","parameters":[{"type":"text","text":"123456"}]}]}`},
		{"an image by ID", WhatsAppMessage{TemplateName: "m", Header: &imageByID},
			`{"name":"m","language":{"code":"en_US"},"components":[{"type":"header","parameters":[{"type":"image","image":{"id":"1234"}}]}]}`},
		{"a video by link", WhatsAppMessage{TemplateName: "m", Header: &video},
			`{"name":"m","language":{"code":"en_US"},"components":[{"type":"header","parameters":[{"type":"video","video":{"link":"https://example.com/v.mp4"}}]}]}`},
		{"a document with its name", WhatsAppMessage{TemplateName: "m", Header: &document},
			`{"name":"m","language":{"code":"en_US"},"components":[{"type":"header","parameters":[
			{"type":"document","document":{"link":"https://example.com/invoice.pdf","filename":"invoice.pdf"}}]}]}`},
		{"a location", WhatsAppMessage{TemplateName: "m", Header: &WhatsAppParameter{Location: &WhatsAppLocation{
			Latitude: 37.44211676562361, Longitude: -122.16155960083124, Name: "Philz Coffee", Address: "101 Forest Ave, Palo Alto, CA 94301",
		}}}, `{"name":"m","language":{"code":"en_US"},"components":[{"type":"header","parameters":[{"type":"location","location":{
			"latitude":"37.44211676562361","longitude":"-122.16155960083124","name":"Philz Coffee","address":"101 Forest Ave, Palo Alto, CA 94301"}}]}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.msg.To, tc.msg.LanguageCode = "+15551234567", "en_US"
			got := sentTemplate(t, tc.msg)
			var want any
			if err := json.Unmarshal([]byte(tc.want), &want); err != nil {
				t.Fatalf("the expected JSON: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				gotJSON, _ := json.Marshal(got)
				wantJSON, _ := json.Marshal(want)
				t.Fatalf("sent  %s\nwant %s", gotJSON, wantJSON)
			}
		})
	}
}
