package providers

// Routing facts that belong in the request record.
//
// The summary line has to say who served a request and why a request was
// refused before it ever reached a provider — otherwise 15% of requests (286 of
// 1938 on a measured day) are unaccountable in the log.

import (
	"context"

	"github.com/whtsky/copilot2api/requestlog"
)

// recordRouting notes which provider was chosen.
func recordRouting(ctx context.Context, providerID, model string) {
	requestlog.Mutate(ctx, func(rec *requestlog.Record) {
		rec.Provider = providerID
		if rec.Model == "" {
			rec.Model = model
		}
	})
}

// recordResolvedModel notes the model id that actually goes upstream, which
// differs from the client's request after alias expansion or prefix stripping.
func recordResolvedModel(ctx context.Context, model string) {
	if model == "" {
		return
	}
	requestlog.Mutate(ctx, func(rec *requestlog.Record) {
		rec.ModelResolved = model
	})
}

// recordRefusal explains a request answered without contacting any provider.
func recordRefusal(ctx context.Context, reason, message string) {
	requestlog.Mutate(ctx, func(rec *requestlog.Record) {
		rec.Reason = reason
		rec.ErrorCode = reason
		rec.Error = message
		rec.UpstreamCalled = false
	})
}

// disabledProviderFor returns the model prefix of a disabled provider that would
// otherwise serve this model, so "unknown model" can be told apart from "you
// turned that provider off".
func (c *Config) disabledProviderFor(model string) string {
	for _, prefix := range c.disabledPrefixes {
		if prefix == "" {
			continue
		}
		if len(model) > len(prefix)+1 && model[:len(prefix)+1] == prefix+"-" {
			return prefix
		}
	}
	return ""
}
