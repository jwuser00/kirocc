package auth

import (
	"encoding/json/v2"
	"strconv"
	"strings"
	"time"
)

// coalesce returns the first non-empty string.
func coalesce(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// parseExpiresAt extracts an int64 timestamp from values that may be int, float, or string.
func parseExpiresAt(vals ...any) int64 {
	for _, v := range vals {
		if v == nil {
			continue
		}
		switch t := v.(type) {
		case float64:
			if t > 0 {
				return int64(t)
			}
		case string:
			if t == "" {
				continue
			}
			// Try parsing as unix timestamp string.
			if n, err := strconv.ParseInt(t, 10, 64); err == nil && n > 0 {
				return n
			}
			if n, err := strconv.ParseFloat(t, 64); err == nil && n > 0 {
				return int64(n)
			}
			// Try parsing as ISO 8601 timestamp.
			if ts, err := time.Parse(time.RFC3339Nano, t); err == nil {
				return ts.Unix()
			}
			if ts, err := time.Parse(time.RFC3339, t); err == nil {
				return ts.Unix()
			}
		}
	}
	return 0
}

// extractProfileARN extracts the ARN from a profile value that may be:
// - a plain ARN string
// - a JSON object like {"arn":"...","profile_name":"..."}
func extractProfileARN(raw string) string {
	if raw == "" {
		return ""
	}
	// Try as JSON object with "arn" field.
	var obj struct {
		ARN string `json:"arn"`
	}
	if err := json.Unmarshal([]byte(raw), &obj); err == nil && obj.ARN != "" {
		return obj.ARN
	}
	return raw
}

// extractRegionFromARN parses the region segment from an AWS ARN.
// ARN format: arn:partition:service:region:account-id:resource
// Returns "" if the input is not a valid ARN with a non-empty region segment.
func extractRegionFromARN(arn string) string {
	if arn == "" {
		return ""
	}
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) < 6 || parts[0] != "arn" {
		return ""
	}
	return parts[3]
}

// resolveRegion picks the first non-empty region in priority order, falling back
// to "us-east-1" when none is set. Intended for Credentials.Region (the API
// region embedded in runtime.<region>.kiro.dev). Do NOT use for SSORegion: the
// token refresh path relies on an empty SSORegion to fail fast when IDC is
// misconfigured, and the fallback would mask that condition.
//
// The CodeWhisperer profile ARN encodes the API region directly, so ARN-derived
// values take priority over any stored region field. Both the token JSON
// "region" and state "auth.idc.region" hold the OIDC/SSO region, which can
// legitimately differ from the API region for IDC users: an Identity Center
// instance in ap-northeast-2 can be issued a profile in us-east-1. Preferring
// the stored region there would build runtime.ap-northeast-2.kiro.dev, which
// does not exist. Stored regions remain as fallbacks for credentials that carry
// no profile ARN, where they are the only signal available.
func resolveRegion(tokenRegion, tokenProfileARN, stateRegion, stateProfileARN string) string {
	candidates := []string{
		extractRegionFromARN(tokenProfileARN),
		extractRegionFromARN(stateProfileARN),
		tokenRegion,
		stateRegion,
	}
	for _, r := range candidates {
		if r != "" {
			return r
		}
	}
	return "us-east-1"
}
