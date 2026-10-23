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
	"regexp"
	"strconv"
	"strings"
	"time"
)

const metaGraphAPIBaseURL = "https://graph.facebook.com"

// defaultMetaGraphAPIVersion is MetaCloudDispatcherDeps.GraphAPIVersion's
// default: v26.0, the newest Graph API version (released 2026-07-29), read
// from Meta's changelog on 2026-10-23. Meta keeps each version about two
// years; v20.0, hard-coded here until then, stopped being available on
// 2026-09-24. Bump it when Meta announces the next one's end date.
const defaultMetaGraphAPIVersion = "v26.0"

// metaGraphAPIVersionPattern is the shape of a Graph API version: "v",
// major, ".", minor.
var metaGraphAPIVersionPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+$`)

// metaTemporaryErrorCodes are the Graph API error codes Meta's WhatsApp
// Cloud API error reference says to retry after a wait (read 2026-10-23):
// 2 (temporary downtime or overload), 4 (the app's call rate limit), 80007
// (the account's rate limit), 130429 (message throughput limit), 131000
// (unknown error), 131016 (service temporarily unavailable), 131056 (too
// many messages to one recipient), 133004 (server temporarily
// unavailable), 2494100 (the number in maintenance mode). doJSON treats
// these, a 429 and any 5xx as temporary, and every other refusal (bad
// parameters, permissions, an expired token, an unknown or paused
// template, an undeliverable number, policy) as permanent.
var metaTemporaryErrorCodes = map[int]bool{
	2: true, 4: true, 80007: true, 130429: true, 131000: true,
	131016: true, 131056: true, 133004: true, 2494100: true,
}

// WhatsAppCloudAPIRequest is grpop's own vendor-neutral shape for one
// WhatsApp template send, translated from a WhatsAppMessage by
// metaCloudDispatcher (after validateWhatsAppMessage) before reaching
// WhatsAppCloudAPIClient.
type WhatsAppCloudAPIRequest struct {
	To           string
	TemplateName string
	LanguageCode string
	Header       *WhatsAppParameter
	Body         []WhatsAppParameter
	Buttons      []WhatsAppButton
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

	// GraphAPIVersion is the Graph API version every call names ("v26.0").
	// Empty uses defaultMetaGraphAPIVersion. Meta retires a version about
	// two years after release, so an operator can move ahead of grpop's
	// default without a new grpop release.
	GraphAPIVersion string

	// Client is optional; nil constructs a real net/http-backed one from
	// PhoneNumberID/BusinessAccountID/AccessToken/RequestTimeout.
	Client            WhatsAppCloudAPIClient
	TemplateValidator TemplateValidator
	// RateLimiter bounds the whole channel and RecipientRateLimiter each
	// recipient (see RateLimiter's doc comment); both optional. A refusal
	// is ErrRateLimited at once: Send never waits for a token.
	RateLimiter          RateLimiter
	RecipientRateLimiter RateLimiter
	CircuitBreaker       CircuitBreaker
	Metrics              Metrics
	Logger               Logger
}

// metaCloudDispatcher implements WhatsAppSender over Meta's Graph API.
type metaCloudDispatcher struct {
	client            WhatsAppCloudAPIClient
	templateValidator TemplateValidator
	rateLimiter       RateLimiter
	recipientLimit    RateLimiter
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
		version := deps.GraphAPIVersion
		if version == "" {
			version = defaultMetaGraphAPIVersion
		}
		if !metaGraphAPIVersionPattern.MatchString(version) {
			return nil, fmt.Errorf("grpop/metacloud: GraphAPIVersion %q is not a Graph API version like v26.0", version)
		}
		client = newRealWhatsAppCloudAPIClient(deps.PhoneNumberID, deps.BusinessAccountID, deps.AccessToken, version, timeout)
	}

	return &metaCloudDispatcher{
		client:            client,
		templateValidator: deps.TemplateValidator,
		rateLimiter:       deps.RateLimiter,
		recipientLimit:    deps.RecipientRateLimiter,
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
		if err := d.templateValidator.Validate(ctx, msg); err != nil {
			d.logger.Warn("grpop/metacloud: template validation failed", "template", msg.TemplateName, "error", err)
			if d.metrics != nil {
				d.metrics.IncSendResult(ChannelWhatsApp, SendStatusFailed)
			}
			return SendResult{Channel: ChannelWhatsApp, Status: SendStatusFailed, SentAt: time.Now().UTC()}, err
		}
	}

	if err := checkRateLimits(ctx, ChannelWhatsApp, msg.To, d.rateLimiter, d.recipientLimit, d.metrics); err != nil {
		return SendResult{}, err
	}

	// The same fields as WhatsAppMessage, so a conversion: a field added to
	// one and not the other stops this compiling.
	req := WhatsAppCloudAPIRequest(msg)

	var resp WhatsAppCloudAPIResponse
	sendFn := func() error {
		var sendErr error
		resp, sendErr = d.client.SendTemplateMessage(ctx, req)
		return sendErr
	}
	err := executeThroughBreaker(ctx, d.circuitBreaker, sendFn)

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
	apiVersion        string
	// baseURL defaults to metaGraphAPIBaseURL; overridable only by tests, so
	// doJSON's request-building/response/error-envelope-parsing logic can be
	// exercised against a real local httptest.Server instead of Meta itself.
	baseURL string
}

var _ WhatsAppCloudAPIClient = (*metaCloudAPIClient)(nil)

func newRealWhatsAppCloudAPIClient(phoneNumberID, businessAccountID, accessToken, apiVersion string, timeout time.Duration) *metaCloudAPIClient {
	return &metaCloudAPIClient{
		httpClient:        &http.Client{Timeout: timeout},
		phoneNumberID:     phoneNumberID,
		businessAccountID: businessAccountID,
		accessToken:       accessToken,
		apiVersion:        apiVersion,
		baseURL:           metaGraphAPIBaseURL,
	}
}

type metaLanguage struct {
	Code string `json:"code"`
}

// metaTemplateParameter is one value of a send's components, holding the
// field its Type names.
type metaTemplateParameter struct {
	Type          string        `json:"type"`
	ParameterName string        `json:"parameter_name,omitempty"`
	Text          string        `json:"text,omitempty"`
	Currency      *metaCurrency `json:"currency,omitempty"`
	DateTime      *metaDateTime `json:"date_time,omitempty"`
	Image         *metaMedia    `json:"image,omitempty"`
	Video         *metaMedia    `json:"video,omitempty"`
	Document      *metaMedia    `json:"document,omitempty"`
	Location      *metaLocation `json:"location,omitempty"`
	Payload       string        `json:"payload,omitempty"`
	CouponCode    string        `json:"coupon_code,omitempty"`
}

type metaCurrency struct {
	FallbackValue string `json:"fallback_value"`
	Code          string `json:"code"`
	Amount1000    int64  `json:"amount_1000"`
}

type metaDateTime struct {
	FallbackValue string `json:"fallback_value"`
}

type metaMedia struct {
	ID       string `json:"id,omitempty"`
	Link     string `json:"link,omitempty"`
	Filename string `json:"filename,omitempty"`
}

// metaLocation's coordinates are strings, as in Meta's own location header
// example.
type metaLocation struct {
	Latitude  string `json:"latitude"`
	Longitude string `json:"longitude"`
	Name      string `json:"name,omitempty"`
	Address   string `json:"address,omitempty"`
}

// metaTemplateComponent is a send's header, body or one button. A button
// names its sub_type and its index, a string ("0").
type metaTemplateComponent struct {
	Type       string                  `json:"type"`
	SubType    string                  `json:"sub_type,omitempty"`
	Index      string                  `json:"index,omitempty"`
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
			Name:       req.TemplateName,
			Language:   metaLanguage{Code: req.LanguageCode},
			Components: metaTemplateComponents(req),
		},
	}

	url := fmt.Sprintf("%s/%s/%s/messages", c.baseURL, c.apiVersion, c.phoneNumberID)
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

// metaTemplateComponents is req's header, body and buttons as Meta's send
// takes them: a header component with its one value, a body component with
// its values in order, and one button component per button. A template
// with nothing to fill has no components (nil, left out of the JSON).
func metaTemplateComponents(req WhatsAppCloudAPIRequest) []metaTemplateComponent {
	var components []metaTemplateComponent
	if req.Header != nil {
		components = append(components, metaTemplateComponent{
			Type: "header", Parameters: []metaTemplateParameter{metaParameter(*req.Header)},
		})
	}
	if len(req.Body) > 0 {
		params := make([]metaTemplateParameter, 0, len(req.Body))
		for _, p := range req.Body {
			params = append(params, metaParameter(p))
		}
		components = append(components, metaTemplateComponent{Type: "body", Parameters: params})
	}
	for _, b := range req.Buttons {
		components = append(components, metaButton(b))
	}
	return components
}

// metaParameter is p as Meta's typed parameter; p holds one value
// (validateWhatsAppMessage), and a text value is the fallthrough.
func metaParameter(p WhatsAppParameter) metaTemplateParameter {
	param := metaTemplateParameter{ParameterName: p.Name}
	switch {
	case p.Currency != nil:
		param.Type = "currency"
		param.Currency = &metaCurrency{FallbackValue: p.Currency.FallbackValue, Code: p.Currency.Code, Amount1000: p.Currency.Amount1000}
	case p.DateTime != nil:
		param.Type = "date_time"
		param.DateTime = &metaDateTime{FallbackValue: p.DateTime.FallbackValue}
	case p.Media != nil:
		media := &metaMedia{ID: p.Media.ID, Link: p.Media.Link, Filename: p.Media.Filename}
		param.Type = string(p.Media.Kind)
		switch p.Media.Kind {
		case WhatsAppMediaImage:
			param.Image = media
		case WhatsAppMediaVideo:
			param.Video = media
		default:
			param.Document = media
		}
	case p.Location != nil:
		param.Type = "location"
		param.Location = &metaLocation{
			Latitude:  strconv.FormatFloat(p.Location.Latitude, 'f', -1, 64),
			Longitude: strconv.FormatFloat(p.Location.Longitude, 'f', -1, 64),
			Name:      p.Location.Name,
			Address:   p.Location.Address,
		}
	default:
		param.Type = "text"
		param.Text = p.Text
	}
	return param
}

// metaButton is b as a button component. A one-time code goes as Meta's
// "url" sub_type, as a URL suffix does: that is how Meta's copy-code and
// one-tap authentication templates take it.
func metaButton(b WhatsAppButton) metaTemplateComponent {
	component := metaTemplateComponent{Type: "button", Index: strconv.Itoa(b.Index)}
	switch b.Kind {
	case WhatsAppButtonQuickReply:
		component.SubType = "quick_reply"
		component.Parameters = []metaTemplateParameter{{Type: "payload", Payload: b.Value}}
	case WhatsAppButtonCopyCode:
		component.SubType = "copy_code"
		component.Parameters = []metaTemplateParameter{{Type: "coupon_code", CouponCode: b.Value}}
	default:
		component.SubType = "url"
		component.Parameters = []metaTemplateParameter{{Type: "text", ParameterName: b.Name, Text: b.Value}}
	}
	return component
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
		c.baseURL, c.apiVersion, c.businessAccountID)
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
		code := 0
		var apiErr error
		if json.Unmarshal(respBody, &envelope) == nil && envelope.Error.Message != "" {
			code = envelope.Error.Code
			apiErr = fmt.Errorf("grpop/metacloud: graph api error (status %d, code %d): %s",
				resp.StatusCode, code, envelope.Error.Message)
		} else {
			apiErr = fmt.Errorf("grpop/metacloud: graph api error: status %d", resp.StatusCode)
		}
		if !metaErrorIsTemporary(resp.StatusCode, code) {
			apiErr = permanent(apiErr)
		}
		return nil, apiErr
	}
	return respBody, nil
}

// metaErrorIsTemporary reports whether a Graph API refusal is worth another
// attempt later: one of metaTemporaryErrorCodes, a 429, or a 5xx.
func metaErrorIsTemporary(status, code int) bool {
	return metaTemporaryErrorCodes[code] || status == http.StatusTooManyRequests || status >= 500
}
