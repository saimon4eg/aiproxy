package requestlog

import (
	"strconv"
	"strings"
	"time"
)

// ParseSize accepts a byte count with an optional unit — "20G", "20480M", "512MiB",
// "10737418240" — and returns it in bytes. A bare number is bytes.
//
// An unparsable value falls back rather than being read as zero: for a retention
// limit, "0" means "no limit", and a typo in the configuration must not quietly
// remove the only bound on the log volume.
func ParseSize(raw string, fallback int64) int64 {
	raw = strings.TrimSpace(strings.ToUpper(raw))
	if raw == "" {
		return fallback
	}
	multiplier := int64(1)
	for _, unit := range []struct {
		suffix string
		scale  int64
	}{
		{"TIB", 1 << 40}, {"TB", 1 << 40}, {"GIB", 1 << 30}, {"GB", 1 << 30},
		{"MIB", 1 << 20}, {"MB", 1 << 20}, {"KIB", 1 << 10}, {"KB", 1 << 10},
		{"T", 1 << 40}, {"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10},
		{"B", 1},
	} {
		if strings.HasSuffix(raw, unit.suffix) {
			multiplier = unit.scale
			raw = strings.TrimSpace(strings.TrimSuffix(raw, unit.suffix))
			break
		}
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || value <= 0 {
		return fallback
	}
	return int64(value * float64(multiplier))
}

// ParseDuration accepts Go duration syntax ("24h", "6h30m") and a bare number of
// hours, because a retention interval is naturally written as "24".
func ParseDuration(raw string, fallback time.Duration) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	if parsed, err := time.ParseDuration(raw); err == nil && parsed > 0 {
		return parsed
	}
	if hours, err := strconv.ParseFloat(raw, 64); err == nil && hours > 0 {
		return time.Duration(hours * float64(time.Hour))
	}
	return fallback
}
