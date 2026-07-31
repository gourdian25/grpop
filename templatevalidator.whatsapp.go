// File: templatevalidator.whatsapp.go

package grpop

import (
	"context"
	"errors"
	"fmt"
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

// MetaTemplateValidatorDeps configures NewMetaTemplateValidator.
type MetaTemplateValidatorDeps struct {
	// Client is required — the same WhatsAppCloudAPIClient a
	// metaCloudDispatcher uses; GetApprovedTemplates is the only method this
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
	client WhatsAppCloudAPIClient
	ttl    time.Duration
	logger Logger

	mu        sync.RWMutex
	templates map[string]WhatsAppTemplateInfo // key: templateKey(name, language)
	lastFetch time.Time
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
		client: deps.Client,
		ttl:    ttl,
		logger: OrNop(deps.Logger),
	}, nil
}

func (v *metaTemplateValidator) Validate(ctx context.Context, templateName, languageCode string, variables map[string]string) error {
	if err := v.ensureFresh(ctx); err != nil {
		return err
	}

	v.mu.RLock()
	info, ok := v.templates[templateKey(templateName, languageCode)]
	v.mu.RUnlock()

	if !ok {
		return ErrWhatsAppTemplateNotApproved
	}
	if len(variables) != info.ParameterCount {
		return ErrWhatsAppTemplateArityMismatch
	}
	return nil
}

func (v *metaTemplateValidator) Refresh(ctx context.Context) error {
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
	v.lastFetch = time.Now()
	v.mu.Unlock()

	v.logger.Debug("grpop/templatevalidator: refreshed", "template_count", len(m))
	return nil
}

// ensureFresh triggers a Refresh if the cache has never been populated or
// TTL has elapsed since the last fetch — called from Validate so a caller
// never has to remember to call Refresh themselves for ordinary staleness;
// Refresh remains exposed separately for the "I just approved a new
// template, don't make me wait out the TTL" case.
func (v *metaTemplateValidator) ensureFresh(ctx context.Context) error {
	v.mu.RLock()
	stale := v.templates == nil || time.Since(v.lastFetch) >= v.ttl
	v.mu.RUnlock()
	if !stale {
		return nil
	}
	return v.Refresh(ctx)
}

// templateKey combines a template name and language code into
// metaTemplateValidator's internal cache key — Meta template approval is
// scoped per-language, so the same name can be approved in one language and
// not another.
func templateKey(name, languageCode string) string {
	return name + "|" + languageCode
}
