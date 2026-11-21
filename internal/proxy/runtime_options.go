package proxy

import (
	"fmt"

	"github.com/joeycumines/ai-concurrency-shaper/internal/control"
	"github.com/joeycumines/ai-concurrency-shaper/internal/observation"
)

type observationOption struct {
	hub      *observation.Hub
	provider string
}

// WithObservation attaches the shared observation domain. When the outer router
// already began a logical request, the proxy keeps that ID and transfers its
// active provider scope; it does not count a second received request.
func WithObservation(hub *observation.Hub, provider string) Option {
	return observationOption{hub, provider}
}
func (o observationOption) applyProxyOption(c *proxyConfig) error {
	if o.hub == nil || o.provider == "" {
		return fmt.Errorf("proxy: observation requires a hub and provider name")
	}
	if c.providerName != "" && c.providerName != o.provider {
		return fmt.Errorf("proxy: conflicting provider identities")
	}
	c.observations = o.hub
	c.providerName = o.provider
	return nil
}

type runtimeControlOption struct {
	controller *control.Controller
	provider   string
}

// WithRuntimeControl gates new arrivals before admission and conversion. The
// provider and its existing pools must be registered on the controller first.
func WithRuntimeControl(c *control.Controller, provider string) Option {
	return runtimeControlOption{c, provider}
}
func (o runtimeControlOption) applyProxyOption(c *proxyConfig) error {
	if o.controller == nil || o.provider == "" {
		return fmt.Errorf("proxy: runtime controls require a controller and provider name")
	}
	if c.providerName != "" && c.providerName != o.provider {
		return fmt.Errorf("proxy: conflicting provider identities")
	}
	c.runtimeControl = o.controller
	c.providerName = o.provider
	return nil
}
