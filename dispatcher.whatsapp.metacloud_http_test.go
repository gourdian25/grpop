// File: dispatcher.whatsapp.metacloud_http_test.go

package grpop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// newTestMetaCloudAPIClient is a real metaCloudAPIClient pointed at a local
// httptest.Server instead of graph.facebook.com, through its unexported
// baseURL, so the request building and the response and error parsing run
// over a real HTTP round trip.
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

// TestMetaCloudAPIClient_GetApprovedTemplates_EveryPage: the listing asks
// for approved templates only, follows Meta's cursor page by page on the
// client's own host (never the paging.next URL, which here points nowhere),
// still drops a template that isn't APPROVED, and finds one past page one.
func TestMetaCloudAPIClient_GetApprovedTemplates_EveryPage(t *testing.T) {
	pages := map[string]string{
		"": `{"data":[
			{"name":"approved_one","language":"en_US","status":"APPROVED","components":[{"type":"BODY","text":"Hi {{1}}, order {{2}} shipped"}]},
			{"name":"pending_one","language":"en_US","status":"PENDING","components":[]}
		],"paging":{"cursors":{"before":"b0","after":"c1"},"next":"http://127.0.0.1:1/never?after=c1"}}`,
		"c1": `{"data":[{"name":"no_params","language":"en_US","status":"APPROVED","components":[{"type":"BODY","text":"Hello there"}]}],
			"paging":{"cursors":{"before":"b1","after":"c2"},"next":"http://127.0.0.1:1/never?after=c2"}}`,
		"c2": `{"data":[{"name":"far_template","language":"pt_BR","status":"APPROVED","parameter_format":"NAMED",
			"components":[{"type":"BODY","text":"Olá {{first_name}}"}]}],
			"paging":{"cursors":{"before":"b2","after":"c3"}}}`,
	}
	var queries []url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.Query())
		if r.URL.Path != "/"+defaultMetaGraphAPIVersion+"/test-waba-id/message_templates" || r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("request %s with Authorization %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(pages[r.URL.Query().Get("after")]))
	}))
	defer server.Close()

	templates, err := newTestMetaCloudAPIClient(server.URL).GetApprovedTemplates(context.Background())
	if err != nil {
		t.Fatalf("GetApprovedTemplates: %v", err)
	}
	if len(queries) != 3 {
		t.Fatalf("%d pages asked for, want 3", len(queries))
	}
	for i, q := range queries {
		if q.Get("status") != "APPROVED" || q.Get("limit") != "100" || !strings.Contains(q.Get("fields"), "parameter_format") || !strings.Contains(q.Get("fields"), "components") {
			t.Fatalf("page %d asked %v, want status=APPROVED, limit=100 and the fields read", i, q)
		}
	}
	var names []string
	for _, tpl := range templates {
		names = append(names, tpl.Name)
	}
	if !slices.Equal(names, []string{"approved_one", "no_params", "far_template"}) {
		t.Fatalf("templates = %q, want the three approved ones, page three's included", names)
	}
	if got := templates[0].BodyParameters; !slices.Equal(got, []string{"1", "2"}) {
		t.Fatalf("approved_one.BodyParameters = %q, want [1 2]", got)
	}
	if got := templates[2]; !got.Named || !slices.Equal(got.BodyParameters, []string{"first_name"}) {
		t.Fatalf("far_template = %+v, want named {{first_name}}", got)
	}
}

// TestMetaCloudAPIClient_GetApprovedTemplates_RunawayListing: a listing
// that repeats its cursor, or runs past metaTemplateMaxPages pages, is an
// error, not an endless loop or a cut-off list.
func TestMetaCloudAPIClient_GetApprovedTemplates_RunawayListing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		after func(page int) string
		pages int
	}{
		{"a repeated cursor", func(int) string { return "same" }, 2},
		{"too many pages", func(page int) string { return fmt.Sprint("c", page) }, metaTemplateMaxPages},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asked := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				asked++
				fmt.Fprintf(w, `{"data":[],"paging":{"cursors":{"after":%q},"next":"http://127.0.0.1:1/never"}}`, tc.after(asked))
			}))
			defer server.Close()
			if _, err := newTestMetaCloudAPIClient(server.URL).GetApprovedTemplates(context.Background()); err == nil {
				t.Fatal("GetApprovedTemplates() = nil error, want the runaway refused")
			}
			if asked != tc.pages {
				t.Fatalf("%d pages asked for, want %d", asked, tc.pages)
			}
		})
	}
}

// TestMetaTemplateInfo: each part of a listed template is read as what it
// takes at send time, including a one-time-code button Meta lists as a URL
// and an older template that names its placeholders without saying so.
func TestMetaTemplateInfo(t *testing.T) {
	cases := []struct {
		name, listed string
		want         WhatsAppTemplateInfo
	}{
		{"named, every button kind", `{"name":"n","language":"en_US","status":"APPROVED","parameter_format":"NAMED","components":[
			{"type":"HEADER","format":"TEXT","text":"Order {{order_id}}"},
			{"type":"BODY","text":"Hi {{first_name}}, {{total}} due {{first_name}}"},
			{"type":"FOOTER","text":"Thanks"},
			{"type":"BUTTONS","buttons":[
				{"type":"QUICK_REPLY","text":"Stop"},
				{"type":"URL","text":"Track","url":"https://example.com/track/{{1}}"},
				{"type":"URL","text":"Home","url":"https://example.com/"},
				{"type":"COPY_CODE","example":"SAVE20"},
				{"type":"PHONE_NUMBER","phone_number":"+15551234567"},
				{"type":"OTP","otp_type":"ONE_TAP"}]}]}`,
			WhatsAppTemplateInfo{Name: "n", LanguageCode: "en_US", Named: true, HeaderFormat: "TEXT",
				HeaderParameters: []string{"order_id"}, BodyParameters: []string{"first_name", "total"},
				Buttons: []WhatsAppTemplateButton{
					{Index: 0, Type: "QUICK_REPLY"},
					{Index: 1, Type: "URL", RequiresValue: true},
					{Index: 2, Type: "URL"},
					{Index: 3, Type: "COPY_CODE", RequiresValue: true},
					{Index: 4, Type: "PHONE_NUMBER"},
					{Index: 5, Type: "OTP", RequiresValue: true},
				}}},
		{"an authentication template as Meta lists it", `{"name":"login_code","language":"en_US","status":"APPROVED","category":"AUTHENTICATION","components":[
			{"type":"BODY","text":"*{{1}}* is your verification code."},
			{"type":"FOOTER","text":"This code expires in 10 minutes."},
			{"type":"BUTTONS","buttons":[{"type":"URL","text":"Copy code","url":"https://www.whatsapp.com/otp/code/?otp_type=COPY_CODE&code=otp{{1}}"}]}]}`,
			WhatsAppTemplateInfo{Name: "login_code", LanguageCode: "en_US", BodyParameters: []string{"1"},
				Buttons: []WhatsAppTemplateButton{{Index: 0, Type: "URL", RequiresValue: true}}}},
		{"a one-time-code URL listed with its otp_type and no placeholder", `{"name":"o","language":"en","status":"APPROVED","components":[
			{"type":"BODY","text":"{{1}} is your code."},
			{"type":"BUTTONS","buttons":[{"type":"URL","otp_type":"COPY_CODE","url":"https://www.whatsapp.com/otp/code/"}]}]}`,
			WhatsAppTemplateInfo{Name: "o", LanguageCode: "en", BodyParameters: []string{"1"},
				Buttons: []WhatsAppTemplateButton{{Index: 0, Type: "URL", RequiresValue: true}}}},
		{"named placeholders and no parameter_format", `{"name":"old","language":"en","status":"APPROVED","components":[
			{"type":"body","text":"Hi {{first_name}}"}]}`,
			WhatsAppTemplateInfo{Name: "old", LanguageCode: "en", Named: true, BodyParameters: []string{"first_name"}}},
		{"positional, an image header", `{"name":"p","language":"en","status":"APPROVED","parameter_format":"POSITIONAL","components":[
			{"type":"HEADER","format":"IMAGE","example":{"header_handle":["4::aW"]}},
			{"type":"BODY","text":"{{1}} and {{2}}"}]}`,
			WhatsAppTemplateInfo{Name: "p", LanguageCode: "en", HeaderFormat: "IMAGE", BodyParameters: []string{"1", "2"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var listed metaTemplate
			if err := json.Unmarshal([]byte(tc.listed), &listed); err != nil {
				t.Fatalf("the listed JSON: %v", err)
			}
			if got := metaTemplateInfo(listed); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("metaTemplateInfo =\n %+v\nwant\n %+v", got, tc.want)
			}
		})
	}
	otpBody, otpButtons := WhatsAppOTP("123456")
	var auth metaTemplate
	_ = json.Unmarshal([]byte(cases[1].listed), &auth)
	if err := templateFits(metaTemplateInfo(auth), WhatsAppMessage{Body: otpBody, Buttons: otpButtons}); err != nil {
		t.Fatalf("WhatsAppOTP against the listed authentication template = %v, want it to fit", err)
	}
}

// TestNewMetaCloudAPIClient: a token and an ID of digits are required, and
// the one client serves a sender and a validator alike.
func TestNewMetaCloudAPIClient(t *testing.T) {
	for _, cfg := range []MetaCloudAPIClientConfig{
		{PhoneNumberID: "123"},
		{AccessToken: "tok"},
		{AccessToken: "tok", PhoneNumberID: "123/../456"},
		{AccessToken: "tok", BusinessAccountID: "waba"},
		{AccessToken: "tok", PhoneNumberID: "123", GraphAPIVersion: "latest"},
	} {
		if _, err := NewMetaCloudAPIClient(cfg); err == nil {
			t.Errorf("NewMetaCloudAPIClient(%+v) = nil error", cfg)
		}
	}

	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/message_templates") {
			_, _ = w.Write([]byte(`{"data":[{"name":"hello","language":"en_US","status":"APPROVED","components":[{"type":"BODY","text":"Hi {{1}}"}]}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"messages":[{"id":"wamid.1"}]}`))
	}))
	defer server.Close()
	client, err := NewMetaCloudAPIClient(MetaCloudAPIClientConfig{PhoneNumberID: "111", BusinessAccountID: "222", AccessToken: "tok"})
	if err != nil {
		t.Fatalf("NewMetaCloudAPIClient: %v", err)
	}
	client.(*metaCloudAPIClient).baseURL = server.URL
	validator, err := NewMetaTemplateValidator(MetaTemplateValidatorDeps{Client: client})
	if err != nil {
		t.Fatalf("NewMetaTemplateValidator: %v", err)
	}
	sender, err := NewMetaCloudWhatsAppDispatcher(MetaCloudDispatcherDeps{Client: client, TemplateValidator: validator})
	if err != nil {
		t.Fatalf("NewMetaCloudWhatsAppDispatcher: %v", err)
	}
	msg := WhatsAppMessage{To: "+15551234567", TemplateName: "hello", LanguageCode: "en_US", Body: []WhatsAppParameter{WhatsAppText("Ada")}}
	if _, err := sender.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	msg.Body = nil
	if _, err := sender.Send(context.Background(), msg); !errors.Is(err, ErrWhatsAppTemplateArityMismatch) {
		t.Fatalf("Send(no value) = %v, want ErrWhatsAppTemplateArityMismatch", err)
	}
	want := []string{"/" + defaultMetaGraphAPIVersion + "/222/message_templates", "/" + defaultMetaGraphAPIVersion + "/111/messages"}
	if !slices.Equal(paths, want) {
		t.Fatalf("calls %q, want the listing then one send %q", paths, want)
	}

	sendOnly, _ := NewMetaCloudAPIClient(MetaCloudAPIClientConfig{BusinessAccountID: "222", AccessToken: "tok"})
	if _, err := sendOnly.SendTemplateMessage(context.Background(), WhatsAppCloudAPIRequest{To: "1"}); !errors.Is(err, ErrPermanent) {
		t.Fatalf("a send with no PhoneNumberID = %v, want a permanent failure", err)
	}
}

// TestMetaCloud_Live sends Meta's own hello_world sample template (en_US,
// no values) for real. It runs only with GRPOP_TEST_META_PHONE_NUMBER_ID,
// GRPOP_TEST_META_ACCESS_TOKEN and GRPOP_TEST_META_TO (a number the account
// may message) set; with GRPOP_TEST_META_BUSINESS_ACCOUNT_ID too, it first
// lists the account's templates for real and validates the send.
func TestMetaCloud_Live(t *testing.T) {
	phoneNumberID, token, to := os.Getenv("GRPOP_TEST_META_PHONE_NUMBER_ID"), os.Getenv("GRPOP_TEST_META_ACCESS_TOKEN"), os.Getenv("GRPOP_TEST_META_TO")
	if phoneNumberID == "" || token == "" || to == "" {
		t.Skip("GRPOP_TEST_META_PHONE_NUMBER_ID, GRPOP_TEST_META_ACCESS_TOKEN and GRPOP_TEST_META_TO not all set")
	}
	businessAccountID := os.Getenv("GRPOP_TEST_META_BUSINESS_ACCOUNT_ID")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	client, err := NewMetaCloudAPIClient(MetaCloudAPIClientConfig{PhoneNumberID: phoneNumberID, BusinessAccountID: businessAccountID, AccessToken: token})
	if err != nil {
		t.Fatalf("NewMetaCloudAPIClient: %v", err)
	}
	deps := MetaCloudDispatcherDeps{Client: client}
	msg := WhatsAppMessage{To: to, TemplateName: "hello_world", LanguageCode: "en_US"}
	if businessAccountID != "" {
		templates, err := client.GetApprovedTemplates(ctx)
		if err != nil {
			t.Fatalf("GetApprovedTemplates: %v", err)
		}
		t.Logf("%d approved templates listed", len(templates))
		validator, err := NewMetaTemplateValidator(MetaTemplateValidatorDeps{Client: client})
		if err != nil {
			t.Fatalf("NewMetaTemplateValidator: %v", err)
		}
		if err := validator.Refresh(ctx); err != nil {
			t.Fatalf("Refresh: %v", err)
		}
		if err := validator.Validate(ctx, msg); err != nil {
			t.Fatalf("Validate(hello_world): %v", err)
		}
		deps.TemplateValidator = validator
	}
	sender, err := NewMetaCloudWhatsAppDispatcher(deps)
	if err != nil {
		t.Fatalf("NewMetaCloudWhatsAppDispatcher: %v", err)
	}
	result, err := sender.Send(ctx, msg)
	if err != nil {
		t.Fatalf("Send(hello_world): %v", err)
	}
	if !strings.HasPrefix(result.ProviderMessageID, "wamid.") {
		t.Fatalf("ProviderMessageID = %q, want Meta's wamid", result.ProviderMessageID)
	}
	t.Logf("Meta accepted hello_world: %s", result.ProviderMessageID)
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
		sender, err := NewMetaCloudWhatsAppDispatcher(MetaCloudDispatcherDeps{PhoneNumberID: "123", AccessToken: "tok", GraphAPIVersion: tc.configured})
		if err != nil {
			t.Fatalf("NewMetaCloudWhatsAppDispatcher(%q): %v", tc.configured, err)
		}
		sender.(*metaCloudDispatcher).client.(*metaCloudAPIClient).baseURL = server.URL
		if _, err := sender.Send(context.Background(), validWhatsAppMessage()); err != nil {
			t.Fatalf("Send: %v", err)
		}
		server.Close()
		if gotPath != "/"+tc.want+"/123/messages" {
			t.Fatalf("GraphAPIVersion %q called %q, want /%s/123/messages", tc.configured, gotPath, tc.want)
		}
	}
	if defaultMetaGraphAPIVersion == "v20.0" {
		t.Fatal("the default is v20.0, which Meta stopped serving on 2026-09-24")
	}
	for _, bad := range []string{"26.0", "v26", "latest", "v26.0/../x"} {
		if _, err := NewMetaCloudWhatsAppDispatcher(MetaCloudDispatcherDeps{PhoneNumberID: "123", AccessToken: "tok", GraphAPIVersion: bad}); err == nil {
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
	sender, err := NewMetaCloudWhatsAppDispatcher(MetaCloudDispatcherDeps{PhoneNumberID: "123", AccessToken: "tok"})
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
