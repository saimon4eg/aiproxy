package modelid

import "regexp"

var displayContextSuffixRe = regexp.MustCompile(`(?i)\[(?:1m|[0-9]+(?:\.[0-9]+)?[km])\]$`)

// StripDisplayContextSuffixes removes UI-only trailing context suffixes such as
// [1m], [200k] or [1000k]. The operation is repeated to tolerate nested values
// like "model[1000k][1m]" coming from the CC GUI toggle flow.
func StripDisplayContextSuffixes(modelID string) string {
	for modelID != "" {
		stripped := displayContextSuffixRe.ReplaceAllString(modelID, "")
		if stripped == modelID {
			return modelID
		}
		modelID = stripped
	}
	return modelID
}
