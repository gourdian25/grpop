// File: dispatcher.whatsapp.metacloud.go

package grpop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	metaGraphAPIBaseURL = "https://graph.facebook.com"
	metaGraphAPIVersion = "v20.0"
)

// WhatsAppCloudAPIRequest is grpop's own vendor-neutral shape for one
// WhatsApp template send, translated from a WhatsAppMessage by
// metaCloudDispatcher before reaching WhatsAppCloudAPIClient.
type WhatsAppCloudAPIRequest struct {
	To                string
	TemplateName      string
	LanguageCode      string
	TemplateVariables map[string]string
}

// WhatsAppCloudAPIResponse is the vendor-neutral result of one successful
// WhatsAppCloudAPIClient.SendTemplateMessage call.
type WhatsAppCloudAPIResponse struct {
	// MessageID is Meta's own wamid for the sent message.
	MessageID string
}

// WhatsAppCloudAPIClient is grpop's own thin interface over Meta's Graph
// API messages endpoint — no official Go SDK exists for this, so this is a
// hand-rolled HTTP client, kept narrow for fakeability. This is the one
// deliberate exception to grpop's real-services testing policy: Meta's
// Graph API has no local emulator, so metaCloudDispatcher's own retry/
// error-classification/rate-limiter/circuit-breaker wiring is tested
// against a fake implementation of this interface instead.
type WhatsAppCloudAPIClient interface {
	SendTemplateMessage(ctx context.Context, req WhatsAppCloudAPIRequest) (WhatsAppCloudAPIResponse, error)
	// GetApprovedTemplates backs TemplateValidator (templatevalidator.whatsapp.go).
	GetApprovedTemplates(ctx context.Context) ([]WhatsAppTemplateInfo, error)
}

// MetaCloudDispatcherDeps configures NewMetaCloudWhatsAppDispatcher.
type MetaCloudDispatcherDeps struct {
	// PhoneNumberID is Meta's phone-number-id path segment for the messages
	// endpoint. Required unless Client is supplied directly.
	PhoneNumberID string

	// BusinessAccountID is Meta's WhatsApp Business Account ID — a distinct
	// ID from PhoneNumberID, used only for GetApprovedTemplates (message
	// templates belong to the business account, not the phone number). Not
	// part of the original plan's Deps field list — added during
	// implementation because GetApprovedTemplates has no other way to know
	// which account to list templates for (see docs/plan/grpop-plan.md's
	// Stage 11 revision note). Required only if a TemplateValidator backed
	// by this client is actually used (Stage 12); Send never needs it.
	BusinessAccountID string

	// AccessToken is a long-lived/System User access token. grpop does NOT
	// refresh or rotate this token — token lifecycle is entirely the
	// operator's responsibility. Required unless Client is supplied
	// directly.
	AccessToken string

	// RequestTimeout bounds one Graph API HTTP call, applying in addition
	// to ctx's own deadline, whichever is sooner. Default: 10s.
	RequestTimeout time.Duration

	// Client is optional; nil constructs a real net/http-backed one from
	// PhoneNumberID/BusinessAccountID/AccessToken/RequestTimeout.
	Client            WhatsAppCloudAPIClient
	TemplateValidator TemplateValidator
	RateLimiter       RateLimiter
	CircuitBreaker    CircuitBreaker
	Metrics           Metrics
	Logger            Logger
}

// metaCloudDispatcher implements WhatsAppSender over Meta's Graph API.
type metaCloudDispatcher struct {
	client            WhatsAppCloudAPIClient
	templateValidator TemplateValidator
	rateLimiter       RateLimiter
	circuitBreaker    CircuitBreaker
	metrics           Metrics
	logger            Logger
}

var _ WhatsAppSender = (*metaCloudDispatcher)(nil)

// NewMetaCloudWhatsAppDispatcher constructs a WhatsAppSender backed by
// Meta's WhatsApp Business Cloud API.
//
// Parameters:
//   - deps: MetaCloudDispatcherDeps — deps.PhoneNumberID and
//     deps.AccessToken are required unless deps.Client is supplied directly
//
// Returns:
//   - WhatsAppSender
//   - error: non-nil if deps.Client is nil and PhoneNumberID/AccessToken
//     are not both set
func NewMetaCloudWhatsAppDispatcher(deps MetaCloudDispatcherDeps) (WhatsAppSender, error) {
	client := deps.Client
	if client == nil {
		if deps.PhoneNumberID == "" {
			return nil, errors.New("grpop/metacloud: MetaCloudDispatcherDeps.PhoneNumberID is required")
		}
		if deps.AccessToken == "" {
			return nil, errors.New("grpop/metacloud: MetaCloudDispatcherDeps.AccessToken is required")
		}
		timeout := deps.RequestTimeout
		if timeout <= 0 {
			timeout = 10 * time.Second
		}
		client = newRealWhatsAppCloudAPIClient(deps.PhoneNumberID, deps.BusinessAccountID, deps.AccessToken, timeout)
	}

	return &metaCloudDispatcher{
		client:            client,
		templateValidator: deps.TemplateValidator,
		rateLimiter:       deps.RateLimiter,
		circuitBreaker:    deps.CircuitBreaker,
		metrics:           deps.Metrics,
		logger:            OrNop(deps.Logger),
	}, nil
}

func (d *metaCloudDispatcher) Send(ctx context.Context, msg WhatsAppMessage) (SendResult, error) {
	start := time.Now()
	if err := ctx.Err(); err != nil {
		return SendResult{}, err
	}
	if err := validateWhatsAppMessage(msg); err != nil {
		return SendResult{}, err
	}

	if d.templateValidator != nil {
		if err := d.templateValidator.Validate(ctx, msg.TemplateName, msg.LanguageCode, msg.TemplateVariables); err != nil {
			d.logger.Warn("grpop/metacloud: template validation failed", "template", msg.TemplateName, "error", err)
			if d.metrics != nil {
				d.metrics.IncSendResult(ChannelWhatsApp, SendStatusFailed)
			}
			return SendResult{Channel: ChannelWhatsApp, Status: SendStatusFailed, SentAt: time.Now().UTC()}, err
		}
	}

	if d.rateLimiter != nil {
		if err := d.rateLimiter.Wait(ctx, ChannelWhatsApp, msg.To); err != nil {
			return SendResult{}, fmt.Errorf("grpop/metacloud: rate limiter wait: %w", err)
		}
	}

	req := WhatsAppCloudAPIRequest{
		To:                msg.To,
		TemplateName:      msg.TemplateName,
		LanguageCode:      msg.LanguageCode,
		TemplateVariables: msg.TemplateVariables,
	}

	var resp WhatsAppCloudAPIResponse
	sendFn := func() error {
		var sendErr error
		resp, sendErr = d.client.SendTemplateMessage(ctx, req)
		return sendErr
	}

	var err error
	if d.circuitBreaker != nil {
		err = d.circuitBreaker.Execute(ctx, sendFn)
	} else {
		err = sendFn()
	}

	if d.metrics != nil {
		d.metrics.ObserveSendLatency(ChannelWhatsApp, time.Since(start))
	}

	if err != nil {
		d.logger.Error("grpop/metacloud: send failed", "to", maskPhoneNumber(msg.To), "error", err)
		if d.metrics != nil {
			d.metrics.IncSendResult(ChannelWhatsApp, SendStatusFailed)
		}
		return SendResult{Channel: ChannelWhatsApp, Status: SendStatusFailed, SentAt: time.Now().UTC()}, err
	}

	d.logger.Info("grpop/metacloud: sent", "to", maskPhoneNumber(msg.To), "message_id", resp.MessageID)
	if d.metrics != nil {
		d.metrics.IncSendResult(ChannelWhatsApp, SendStatusSent)
	}
	return SendResult{
		Channel:           ChannelWhatsApp,
		ProviderMessageID: resp.MessageID,
		Status:            SendStatusSent,
		SentAt:            time.Now().UTC(),
	}, nil
}

func (d *metaCloudDispatcher) Close() error { return nil }

// maskPhoneNumber masks a phone number for logging, showing only its last 4
// digits.
func maskPhoneNumber(phone string) string {
	if len(phone) <= 4 {
		return "***"
	}
	return "***" + phone[len(phone)-4:]
}

// --- Real Meta Graph API client ---

type metaCloudAPIClient struct {
	httpClient        *http.Client
	phoneNumberID     string
	businessAccountID string
	accessToken       string
	// baseURL defaults to metaGraphAPIBaseURL; overridable only by tests, so
	// doJSON's request-building/response/error-envelope-parsing logic can be
	// exercised against a real local httptest.Server instead of Meta itself.
	baseURL string
}

var _ WhatsAppCloudAPIClient = (*metaCloudAPIClient)(nil)

func newRealWhatsAppCloudAPIClient(phoneNumberID, businessAccountID, accessToken string, timeout time.Duration) *metaCloudAPIClient {
	return &metaCloudAPIClient{
		httpClient:        &http.Client{Timeout: timeout},
		phoneNumberID:     phoneNumberID,
		businessAccountID: businessAccountID,
		accessToken:       accessToken,
		baseURL:           metaGraphAPIBaseURL,
	}
}

type metaLanguage struct {
	Code string `json:"code"`
}

type metaTemplateParameter struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type metaTemplateComponent struct {
	Type       string                  `json:"type"`
	Parameters []metaTemplateParameter `json:"parameters"`
}

type metaTemplatePayload struct {
	Name       string                  `json:"name"`
	Language   metaLanguage            `json:"language"`
	Components []metaTemplateComponent `json:"components,omitempty"`
}

type metaSendMessageRequest struct {
	MessagingProduct string              `json:"messaging_product"`
	To               string              `json:"to"`
	Type             string              `json:"type"`
	Template         metaTemplatePayload `json:"template"`
}

type metaSendMessageResponse struct {
	Messages []struct {
		ID string `json:"id"`
	} `json:"messages"`
}

type metaErrorEnvelope struct {
	Error struct {
		Message   string `json:"message"`
		Type      string `json:"type"`
		Code      int    `json:"code"`
		FBTraceID string `json:"fbtrace_id"`
	} `json:"error"`
}

func (c *metaCloudAPIClient) SendTemplateMessage(ctx context.Context, req WhatsAppCloudAPIRequest) (WhatsAppCloudAPIResponse, error) {
	body := metaSendMessageRequest{
		MessagingProduct: "whatsapp",
		To:               req.To,
		Type:             "template",
		Template: metaTemplatePayload{
			Name:     req.TemplateName,
			Language: metaLanguage{Code: req.LanguageCode},
		},
	}
	if params := positionalTemplateParameters(req.TemplateVariables); len(params) > 0 {
		body.Template.Components = []metaTemplateComponent{{Type: "body", Parameters: params}}
	}

	url := fmt.Sprintf("%s/%s/%s/messages", c.baseURL, metaGraphAPIVersion, c.phoneNumberID)
	respBody, err := c.doJSON(ctx, http.MethodPost, url, body)
	if err != nil {
		return WhatsAppCloudAPIResponse{}, err
	}

	var parsed metaSendMessageResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return WhatsAppCloudAPIResponse{}, fmt.Errorf("grpop/metacloud: decode send response: %w", err)
	}
	if len(parsed.Messages) == 0 {
		return WhatsAppCloudAPIResponse{}, errors.New("grpop/metacloud: send response contained no message id")
	}
	return WhatsAppCloudAPIResponse{MessageID: parsed.Messages[0].ID}, nil
}

// positionalTemplateParameters converts req.TemplateVariables' stringified
// positional keys ("1","2","3", ...) into Meta's ordered component
// parameter list, stopping at the first missing key — Meta's template
// parameters must be contiguous starting at "1", so a gap can't be
// meaningfully represented as "skip this one."
func positionalTemplateParameters(variables map[string]string) []metaTemplateParameter {
	var params []metaTemplateParameter
	for i := 1; ; i++ {
		v, ok := variables[strconv.Itoa(i)]
		if !ok {
			break
		}
		params = append(params, metaTemplateParameter{Type: "text", Text: v})
	}
	return params
}

type metaTemplateListResponse struct {
	Data []struct {
		Name       string `json:"name"`
		Language   string `json:"language"`
		Status     string `json:"status"`
		Components []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"components"`
	} `json:"data"`
}

func (c *metaCloudAPIClient) GetApprovedTemplates(ctx context.Context) ([]WhatsAppTemplateInfo, error) {
	if c.businessAccountID == "" {
		return nil, errors.New("grpop/metacloud: MetaCloudDispatcherDeps.BusinessAccountID is required to list approved templates")
	}
	url := fmt.Sprintf("%s/%s/%s/message_templates?fields=name,language,status,components&limit=200",
		c.baseURL, metaGraphAPIVersion, c.businessAccountID)
	respBody, err := c.doJSON(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	var parsed metaTemplateListResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("grpop/metacloud: decode template list: %w", err)
	}

	out := make([]WhatsAppTemplateInfo, 0, len(parsed.Data))
	for _, t := range parsed.Data {
		if t.Status != "APPROVED" {
			continue
		}
		paramCount := 0
		for _, comp := range t.Components {
			if comp.Type == "BODY" {
				paramCount = countTemplateParameters(comp.Text)
			}
		}
		out = append(out, WhatsAppTemplateInfo{Name: t.Name, LanguageCode: t.Language, ParameterCount: paramCount})
	}
	return out, nil
}

// countTemplateParameters counts the highest positional placeholder
// ("{{1}}", "{{2}}", ...) referenced in a template body's text, Meta's own
// convention for how many parameters an approved template expects.
func countTemplateParameters(bodyText string) int {
	max := 0
	for i := 1; i <= 20; i++ {
		if strings.Contains(bodyText, "{{"+strconv.Itoa(i)+"}}") {
			max = i
		}
	}
	return max
}

func (c *metaCloudAPIClient) doJSON(ctx context.Context, method, url string, payload any) ([]byte, error) {
	var bodyReader io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("grpop/metacloud: encode request: %w", err)
		}
		bodyReader = bytes.NewReader(encoded)
	}

	httpReq, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("grpop/metacloud: build request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.accessToken)
	if payload != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("grpop/metacloud: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("grpop/metacloud: read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var envelope metaErrorEnvelope
		if json.Unmarshal(respBody, &envelope) == nil && envelope.Error.Message != "" {
			return nil, fmt.Errorf("grpop/metacloud: graph api error (status %d, code %d): %s",
				resp.StatusCode, envelope.Error.Code, envelope.Error.Message)
		}
		return nil, fmt.Errorf("grpop/metacloud: graph api error: status %d", resp.StatusCode)
	}
	return respBody, nil
}
