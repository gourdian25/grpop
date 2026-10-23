// File: templatevalidator.whatsapp.go

package grpop

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// defaultTemplateValidatorTTL is used whenever MetaTemplateValidatorDeps is
// given a TTL <= 0. Templates change rarely (an approval/rejection cycle
// takes minutes to days on Meta's side), so a short-but-not-trivial cache
// window is the right default: cheap enough that a burst of sends doesn't
// each pay a live GetApprovedTemplates round trip, short enough that a
// newly-approved template becomes usable without an explicit Refresh call.
const defaultTemplateValidatorTTL = 5 * time.Minute

// templateListMinInterval is the least time between two listings that
// Validate starts on its own (the TTL's, if shorter, wins): after one that
// failed, so a Meta outage costs a listing per interval and not one per
// send, and for the second look a refusal gets.
const templateListMinInterval = 30 * time.Second

// MetaTemplateValidatorDeps configures NewMetaTemplateValidator.
type MetaTemplateValidatorDeps struct {
	// Client is required — the same WhatsAppCloudAPIClient a
	// metaCloudDispatcher uses (NewMetaCloudAPIClient with a
	// BusinessAccountID); GetApprovedTemplates is the only method this
	// validator calls.
	Client WhatsAppCloudAPIClient
	// TTL bounds how long a fetched approved-template list is trusted
	// before the next Validate call triggers a re-fetch. Defaults to
	// defaultTemplateValidatorTTL if <= 0.
	TTL    time.Duration
	Logger Logger
}

// metaTemplateValidator implements TemplateValidator over a cached
// snapshot of WhatsAppCloudAPIClient.GetApprovedTemplates.
type metaTemplateValidator struct {
	client      WhatsAppCloudAPIClient
	ttl         time.Duration
	minInterval time.Duration // templateListMinInterval, or ttl if shorter
	logger      Logger

	mu          sync.RWMutex
	templates   map[string]WhatsAppTemplateInfo // key: templateKey(name, language); nil until a listing succeeds
	fetchedAt   time.Time                       // the last listing that succeeded
	attemptedAt time.Time                       // the last listing started, whatever came of it
}

var _ TemplateValidator = (*metaTemplateValidator)(nil)

// NewMetaTemplateValidator constructs a TemplateValidator backed by Meta's
// own approved-template list.
//
// Parameters:
//   - deps: MetaTemplateValidatorDeps — deps.Client is required
//
// Returns:
//   - TemplateValidator
//   - error: non-nil if deps.Client is nil
func NewMetaTemplateValidator(deps MetaTemplateValidatorDeps) (TemplateValidator, error) {
	if deps.Client == nil {
		return nil, errors.New("grpop/templatevalidator: MetaTemplateValidatorDeps.Client is required")
	}
	ttl := deps.TTL
	if ttl <= 0 {
		ttl = defaultTemplateValidatorTTL
	}
	return &metaTemplateValidator{
		client:      deps.Client,
		ttl:         ttl,
		minInterval: min(templateListMinInterval, ttl),
		logger:      OrNop(deps.Logger),
	}, nil
}

// Validate checks msg against the newest list it has: a new listing when
// the last is older than the TTL, else the last one that succeeded. It is
// advisory, and Meta stays the authority:
//   - with no list at all (the first listing failed, or another call's is
//     still under way), msg goes through with a warning;
//   - a refusal from a list older than minInterval is checked once more
//     against a new listing before it is returned, since the template may
//     have been approved or edited since.
func (v *metaTemplateValidator) Validate(ctx context.Context, msg WhatsAppMessage) error {
	if v.claimListing(func(have bool, fetchedAt time.Time) bool { return !have || time.Since(fetchedAt) >= v.ttl }) {
		if err := v.Refresh(ctx); err != nil {
			v.logger.Warn("grpop/templatevalidator: listing failed, checking against the last list", "error", err)
		}
	}
	templates := v.snapshot()
	if templates == nil {
		v.logger.Warn("grpop/templatevalidator: no template list, letting the send through", "template", msg.TemplateName)
		return nil
	}
	err := checkTemplate(templates, msg)
	if err != nil && v.claimListing(func(bool, time.Time) bool { return true }) {
		if refreshErr := v.Refresh(ctx); refreshErr == nil {
			err = checkTemplate(v.snapshot(), msg)
		} else {
			v.logger.Warn("grpop/templatevalidator: listing failed, refusing on the last list", "error", refreshErr)
		}
	}
	return err
}

func (v *metaTemplateValidator) Refresh(ctx context.Context) error {
	v.mu.Lock()
	v.attemptedAt = time.Now()
	v.mu.Unlock()

	templates, err := v.client.GetApprovedTemplates(ctx)
	if err != nil {
		return fmt.Errorf("grpop/templatevalidator: refresh: %w", err)
	}

	m := make(map[string]WhatsAppTemplateInfo, len(templates))
	for _, tpl := range templates {
		m[templateKey(tpl.Name, tpl.LanguageCode)] = tpl
	}

	v.mu.Lock()
	v.templates = m
	v.fetchedAt = time.Now()
	v.mu.Unlock()

	v.logger.Debug("grpop/templatevalidator: refreshed", "template_count", len(m))
	return nil
}

// claimListing reports whether this call should list the templates now:
// when due says the list is due and no listing started within the last
// minInterval. A claim records the start, so concurrent calls don't all
// list at once (the others go on with the list there is).
func (v *metaTemplateValidator) claimListing(due func(have bool, fetchedAt time.Time) bool) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !due(v.templates != nil, v.fetchedAt) {
		return false
	}
	if !v.attemptedAt.IsZero() && time.Since(v.attemptedAt) < v.minInterval {
		return false
	}
	v.attemptedAt = time.Now()
	return true
}

// snapshot is the current list (nil when no listing has succeeded). Refresh
// replaces the map whole, so the snapshot is never written again.
func (v *metaTemplateValidator) snapshot() map[string]WhatsAppTemplateInfo {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.templates
}

// checkTemplate finds msg's template in templates and checks msg fills
// what it takes.
func checkTemplate(templates map[string]WhatsAppTemplateInfo, msg WhatsAppMessage) error {
	info, ok := templates[templateKey(msg.TemplateName, msg.LanguageCode)]
	if !ok {
		return fmt.Errorf("%w: %s (%s)", ErrWhatsAppTemplateNotApproved, msg.TemplateName, msg.LanguageCode)
	}
	return templateFits(info, msg)
}

// templateFits returns nil when msg fills exactly what info takes, and
// otherwise ErrWhatsAppTemplateArityMismatch saying what differs (counts,
// placeholder names, kinds; never a value):
//   - the header: a text value for a text header with a placeholder (by
//     name in a named template), the media of a media header's kind, a
//     location for a location header, and nothing for any other header;
//     a format grpop doesn't know (a GIF, one Meta adds) isn't checked;
//   - the body: as many values as placeholders in a positional template,
//     exactly the placeholders' names in a named one;
//   - the buttons: every value fills a button of its kind (a one-time code
//     or a URL suffix a URL or OTP button, a payload a quick reply, a
//     coupon a copy-code button), and every button that requires a value
//     gets one.
func templateFits(info WhatsAppTemplateInfo, msg WhatsAppMessage) error {
	mismatch := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrWhatsAppTemplateArityMismatch, fmt.Sprintf(format, args...))
	}

	h := msg.Header
	switch info.HeaderFormat {
	case "", "TEXT":
		switch {
		case len(info.HeaderParameters) == 0:
			if h != nil {
				return mismatch("the template's header takes no value, the message has one")
			}
		case h == nil || h.Media != nil || h.Location != nil:
			return mismatch("the template's header takes a text value")
		case info.Named && h.Name != info.HeaderParameters[0]:
			return mismatch("the template's header takes {{%s}}", info.HeaderParameters[0])
		}
	case "IMAGE", "VIDEO", "DOCUMENT":
		if h == nil || h.Media == nil || !strings.EqualFold(string(h.Media.Kind), info.HeaderFormat) {
			return mismatch("the template's header takes %s media", strings.ToLower(info.HeaderFormat))
		}
	case "LOCATION":
		if h == nil || h.Location == nil {
			return mismatch("the template's header takes a location")
		}
	}

	var names []string
	for _, p := range msg.Body {
		if (p.Name != "") != info.Named {
			if info.Named {
				return mismatch("the template's values are named, the message's positional")
			}
			return mismatch("the template's values are positional, the message's named")
		}
		names = append(names, p.Name)
	}
	if info.Named {
		want := slices.Sorted(slices.Values(info.BodyParameters))
		slices.Sort(names)
		if !slices.Equal(names, want) {
			return mismatch("the template's body takes %s, the message fills %s", placeholderList(want), placeholderList(names))
		}
	} else if len(msg.Body) != len(info.BodyParameters) {
		return mismatch("the template's body takes %d values, the message has %d", len(info.BodyParameters), len(msg.Body))
	}

	buttons := make(map[int]WhatsAppTemplateButton, len(info.Buttons))
	for _, b := range info.Buttons {
		buttons[b.Index] = b
	}
	filled := make(map[int]bool, len(msg.Buttons))
	for i, b := range msg.Buttons {
		tb, ok := buttons[b.Index]
		if !ok {
			return mismatch("Buttons[%d]: the template has no button %d", i, b.Index)
		}
		if !templateButtonTakes(tb, b.Kind) {
			return mismatch("Buttons[%d]: the template's button %d is %s, which takes no %s value", i, b.Index, tb.Type, b.Kind)
		}
		filled[b.Index] = true
	}
	for _, tb := range info.Buttons {
		if tb.RequiresValue && !filled[tb.Index] {
			return mismatch("the template's button %d (%s) requires a value, the message has none", tb.Index, tb.Type)
		}
	}
	return nil
}

// templateButtonTakes reports whether template button tb takes a message
// button of kind. A one-time code and a URL suffix both go as Meta's url
// sub_type, and Meta lists a created authentication button as a URL, so
// either fills a URL or an OTP button that requires a value.
func templateButtonTakes(tb WhatsAppTemplateButton, kind WhatsAppButtonKind) bool {
	switch kind {
	case WhatsAppButtonURL, WhatsAppButtonOTP:
		return (tb.Type == "URL" || tb.Type == "OTP") && tb.RequiresValue
	case WhatsAppButtonQuickReply:
		return tb.Type == "QUICK_REPLY"
	case WhatsAppButtonCopyCode:
		return tb.Type == "COPY_CODE"
	default:
		return false
	}
}

// placeholderList is names as "{{a}}, {{b}}", or "nothing".
func placeholderList(names []string) string {
	if len(names) == 0 {
		return "nothing"
	}
	return "{{" + strings.Join(names, "}}, {{") + "}}"
}

// templateKey combines a template name and language code into
// metaTemplateValidator's internal cache key — Meta template approval is
// scoped per-language, so the same name can be approved in one language and
// not another.
func templateKey(name, languageCode string) string {
	return name + "|" + languageCode
}
