// File: dispatcher.whatsapp.metacloud_test.go

package grpop

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeWhatsAppCloudAPIClient is a fake WhatsAppCloudAPIClient recording every
// SendTemplateMessage call — this repo's one deliberate exception to real-
// backend testing, since Meta's Graph API has no local emulator.
type fakeWhatsAppCloudAPIClient struct {
	mu sync.Mutex

	sendErr   error
	sendResp  WhatsAppCloudAPIResponse
	sendCalls int
	lastReq   WhatsAppCloudAPIRequest

	templates    []WhatsAppTemplateInfo
	templatesErr error
}

func (c *fakeWhatsAppCloudAPIClient) SendTemplateMessage(_ context.Context, req WhatsAppCloudAPIRequest) (WhatsAppCloudAPIResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sendCalls++
	c.lastReq = req
	if c.sendErr != nil {
		return WhatsAppCloudAPIResponse{}, c.sendErr
	}
	return c.sendResp, nil
}

func (c *fakeWhatsAppCloudAPIClient) GetApprovedTemplates(_ context.Context) ([]WhatsAppTemplateInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.templatesErr != nil {
		return nil, c.templatesErr
	}
	return c.templates, nil
}

func (c *fakeWhatsAppCloudAPIClient) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sendCalls
}

// fakeTemplateValidator is a fake TemplateValidator for asserting
// metaCloudDispatcher consults it (or doesn't, when unset) before sending.
type fakeTemplateValidator struct {
	validateErr   error
	validateCalls int
	refreshCalls  int
}

func (v *fakeTemplateValidator) Validate(_ context.Context, _, _ string, _ map[string]string) error {
	v.validateCalls++
	return v.validateErr
}
func (v *fakeTemplateValidator) Refresh(_ context.Context) error {
	v.refreshCalls++
	return nil
}

func validWhatsAppMessage() WhatsAppMessage {
	return WhatsAppMessage{
		To:                "15551234567",
		TemplateName:      "order_confirmation",
		LanguageCode:      "en_US",
		TemplateVariables: map[string]string{"1": "Ada", "2": "12345"},
	}
}

func TestNewMetaCloudWhatsAppDispatcher_RequiresPhoneNumberIDAndToken(t *testing.T) {
	if _, err := NewMetaCloudWhatsAppDispatcher(MetaCloudDispatcherDeps{}); err == nil {
		t.Fatal("NewMetaCloudWhatsAppDispatcher with no PhoneNumberID/AccessToken/Client = nil error, want non-nil")
	}
	if _, err := NewMetaCloudWhatsAppDispatcher(MetaCloudDispatcherDeps{PhoneNumberID: "123"}); err == nil {
		t.Fatal("NewMetaCloudWhatsAppDispatcher with no AccessToken/Client = nil error, want non-nil")
	}
}

func TestNewMetaCloudWhatsAppDispatcher_ClientAloneIsSufficient(t *testing.T) {
	sender, err := NewMetaCloudWhatsAppDispatcher(MetaCloudDispatcherDeps{Client: &fakeWhatsAppCloudAPIClient{}})
	if err != nil {
		t.Fatalf("NewMetaCloudWhatsAppDispatcher with Client set: %v", err)
	}
	if sender == nil {
		t.Fatal("sender is nil")
	}
}

func TestMetaCloudDispatcher_Send_ValidatesMessage(t *testing.T) {
	client := &fakeWhatsAppCloudAPIClient{}
	sender, err := NewMetaCloudWhatsAppDispatcher(MetaCloudDispatcherDeps{Client: client})
	if err != nil {
		t.Fatalf("NewMetaCloudWhatsAppDispatcher: %v", err)
	}

	tests := []struct {
		name    string
		msg     WhatsAppMessage
		wantErr error
	}{
		{"no recipient", WhatsAppMessage{TemplateName: "t", LanguageCode: "en_US"}, ErrRecipientRequired},
		{"no template name", WhatsAppMessage{To: "15551234567", LanguageCode: "en_US"}, ErrWhatsAppTemplateNameRequired},
		{"no language code", WhatsAppMessage{To: "15551234567", TemplateName: "t"}, ErrWhatsAppLanguageCodeRequired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := sender.Send(context.Background(), tt.msg)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Send() err = %v, want %v", err, tt.wantErr)
			}
		})
	}
	if client.calls() != 0 {
		t.Fatalf("client.calls() = %d, want 0 (validation should reject before any vendor call)", client.calls())
	}
}

func TestMetaCloudDispatcher_Send_Success(t *testing.T) {
	client := &fakeWhatsAppCloudAPIClient{sendResp: WhatsAppCloudAPIResponse{MessageID: "wamid.ABC123"}}
	metrics := &fakeMetrics{}
	sender, err := NewMetaCloudWhatsAppDispatcher(MetaCloudDispatcherDeps{Client: client, Metrics: metrics})
	if err != nil {
		t.Fatalf("NewMetaCloudWhatsAppDispatcher: %v", err)
	}

	result, err := sender.Send(context.Background(), validWhatsAppMessage())
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if result.Status != SendStatusSent {
		t.Fatalf("Status = %v, want SendStatusSent", result.Status)
	}
	if result.ProviderMessageID != "wamid.ABC123" {
		t.Fatalf("ProviderMessageID = %q, want wamid.ABC123", result.ProviderMessageID)
	}
	if result.Channel != ChannelWhatsApp {
		t.Fatalf("Channel = %v, want ChannelWhatsApp", result.Channel)
	}
	if client.lastReq.TemplateName != "order_confirmation" {
		t.Fatalf("lastReq.TemplateName = %q, want order_confirmation", client.lastReq.TemplateName)
	}
	if len(metrics.sendResults) != 1 || metrics.sendResults[0] != SendStatusSent {
		t.Fatalf("metrics.sendResults = %v, want [sent]", metrics.sendResults)
	}
}

func TestMetaCloudDispatcher_Send_VendorError(t *testing.T) {
	client := &fakeWhatsAppCloudAPIClient{sendErr: errors.New("graph api: invalid parameter")}
	sender, err := NewMetaCloudWhatsAppDispatcher(MetaCloudDispatcherDeps{Client: client})
	if err != nil {
		t.Fatalf("NewMetaCloudWhatsAppDispatcher: %v", err)
	}

	result, sendErr := sender.Send(context.Background(), validWhatsAppMessage())
	if sendErr == nil {
		t.Fatal("Send() err = nil, want non-nil")
	}
	if result.Status != SendStatusFailed {
		t.Fatalf("Status = %v, want SendStatusFailed", result.Status)
	}
}

func TestMetaCloudDispatcher_Send_TemplateValidatorRejectsBeforeVendorCall(t *testing.T) {
	client := &fakeWhatsAppCloudAPIClient{}
	validator := &fakeTemplateValidator{validateErr: ErrWhatsAppTemplateNotApproved}
	sender, err := NewMetaCloudWhatsAppDispatcher(MetaCloudDispatcherDeps{Client: client, TemplateValidator: validator})
	if err != nil {
		t.Fatalf("NewMetaCloudWhatsAppDispatcher: %v", err)
	}

	_, sendErr := sender.Send(context.Background(), validWhatsAppMessage())
	if !errors.Is(sendErr, ErrWhatsAppTemplateNotApproved) {
		t.Fatalf("Send() err = %v, want ErrWhatsAppTemplateNotApproved", sendErr)
	}
	if validator.validateCalls != 1 {
		t.Fatalf("validateCalls = %d, want 1", validator.validateCalls)
	}
	if client.calls() != 0 {
		t.Fatalf("client.calls() = %d, want 0 (an unapproved template should never reach the vendor)", client.calls())
	}
}

func TestMetaCloudDispatcher_Send_TemplateValidatorPassesThroughToVendorCall(t *testing.T) {
	client := &fakeWhatsAppCloudAPIClient{sendResp: WhatsAppCloudAPIResponse{MessageID: "wamid.OK"}}
	validator := &fakeTemplateValidator{}
	sender, err := NewMetaCloudWhatsAppDispatcher(MetaCloudDispatcherDeps{Client: client, TemplateValidator: validator})
	if err != nil {
		t.Fatalf("NewMetaCloudWhatsAppDispatcher: %v", err)
	}

	result, err := sender.Send(context.Background(), validWhatsAppMessage())
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if result.Status != SendStatusSent {
		t.Fatalf("Status = %v, want SendStatusSent", result.Status)
	}
	if client.calls() != 1 {
		t.Fatalf("client.calls() = %d, want 1", client.calls())
	}
}

func TestMetaCloudDispatcher_Send_RateLimiterBlocksBeforeVendorCall(t *testing.T) {
	client := &fakeWhatsAppCloudAPIClient{}
	limiter := &fakeChannelRateLimiter{waitErr: ErrRateLimited}
	sender, err := NewMetaCloudWhatsAppDispatcher(MetaCloudDispatcherDeps{Client: client, RateLimiter: limiter})
	if err != nil {
		t.Fatalf("NewMetaCloudWhatsAppDispatcher: %v", err)
	}

	_, sendErr := sender.Send(context.Background(), validWhatsAppMessage())
	if !errors.Is(sendErr, ErrRateLimited) {
		t.Fatalf("Send() err = %v, want to wrap ErrRateLimited", sendErr)
	}
	if client.calls() != 0 {
		t.Fatalf("client.calls() = %d, want 0", client.calls())
	}
}

func TestMetaCloudDispatcher_Send_CircuitBreakerOpensAfterFailure(t *testing.T) {
	client := &fakeWhatsAppCloudAPIClient{sendErr: errors.New("boom")}
	cb, err := NewCircuitBreaker(1, time.Hour, time.Hour)
	if err != nil {
		t.Fatalf("NewCircuitBreaker: %v", err)
	}
	sender, err := NewMetaCloudWhatsAppDispatcher(MetaCloudDispatcherDeps{Client: client, CircuitBreaker: cb})
	if err != nil {
		t.Fatalf("NewMetaCloudWhatsAppDispatcher: %v", err)
	}

	msg := validWhatsAppMessage()
	if _, err := sender.Send(context.Background(), msg); err == nil {
		t.Fatal("first Send() err = nil, want non-nil")
	}
	if _, err := sender.Send(context.Background(), msg); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("second Send() err = %v, want ErrCircuitOpen", err)
	}
	if client.calls() != 1 {
		t.Fatalf("client.calls() = %d, want 1 (circuit breaker should have short-circuited the second Send)", client.calls())
	}
}

func TestPositionalTemplateParameters(t *testing.T) {
	params := positionalTemplateParameters(map[string]string{"1": "a", "2": "b", "3": "c"})
	if len(params) != 3 {
		t.Fatalf("len(params) = %d, want 3", len(params))
	}
	if params[0].Text != "a" || params[1].Text != "b" || params[2].Text != "c" {
		t.Fatalf("params = %+v, want ordered a,b,c", params)
	}
}

func TestPositionalTemplateParameters_StopsAtGap(t *testing.T) {
	params := positionalTemplateParameters(map[string]string{"1": "a", "3": "c"})
	if len(params) != 1 {
		t.Fatalf("len(params) = %d, want 1 (positional params must be contiguous from \"1\")", len(params))
	}
}

func TestPositionalTemplateParameters_Empty(t *testing.T) {
	if params := positionalTemplateParameters(nil); len(params) != 0 {
		t.Fatalf("len(params) = %d, want 0", len(params))
	}
}

func TestCountTemplateParameters(t *testing.T) {
	if got := countTemplateParameters("Hello {{1}}, your order {{2}} shipped"); got != 2 {
		t.Fatalf("countTemplateParameters = %d, want 2", got)
	}
	if got := countTemplateParameters("no placeholders here"); got != 0 {
		t.Fatalf("countTemplateParameters = %d, want 0", got)
	}
}

func TestMaskPhoneNumber(t *testing.T) {
	if got := maskPhoneNumber("15551234567"); got != "***4567" {
		t.Fatalf("maskPhoneNumber = %q, want ***4567", got)
	}
	if got := maskPhoneNumber("123"); got != "***" {
		t.Fatalf("maskPhoneNumber = %q, want ***", got)
	}
}

func TestNewMetaCloudWhatsAppDispatcher_ConstructsRealClientFromPhoneNumberIDAndToken(t *testing.T) {
	sender, err := NewMetaCloudWhatsAppDispatcher(MetaCloudDispatcherDeps{
		PhoneNumberID: "123456", AccessToken: "test-token",
	})
	if err != nil {
		t.Fatalf("NewMetaCloudWhatsAppDispatcher: %v", err)
	}
	if err := sender.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
