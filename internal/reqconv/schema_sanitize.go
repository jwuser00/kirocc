package reqconv

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"maps"
	"sync"
)

// lossyWarnSeen tracks which lossy conversions have already been reported.
// A tool schema is a static property of the client, so an unconditional warning
// fires on every single request and buries the rest of the log. Each distinct
// schema is reported once at Warn; repeats drop to Debug.
var lossyWarnSeen sync.Map

// lossyWarnCap bounds lossyWarnSeen so a client sending endlessly varying
// schemas cannot grow it without limit. Past the cap, everything logs at Debug.
const lossyWarnCap = 256

var lossyWarnCount struct {
	mu sync.Mutex
	n  int
}

// warnLossyOnce reports a lossy combinator conversion, at Warn the first time a
// given branch set is seen and at Debug afterwards.
func warnLossyOnce(combinator string, branches []any) {
	msg := "lossy schema conversion: using first branch only"
	attrs := []any{"combinator", combinator, "branches", len(branches)}

	if firstSighting(combinator, branches) {
		slog.Warn(msg, attrs...)
		return
	}
	slog.Debug(msg, attrs...)
}

// firstSighting reports whether this combinator/branch set has not been seen
// before, recording it when there is room left under lossyWarnCap.
func firstSighting(combinator string, branches []any) bool {
	key, err := fingerprint(combinator, branches)
	if err != nil {
		// Unfingerprintable schema: fall back to Debug rather than risk
		// warning on every request.
		return false
	}
	if _, loaded := lossyWarnSeen.Load(key); loaded {
		return false
	}

	lossyWarnCount.mu.Lock()
	defer lossyWarnCount.mu.Unlock()
	if _, loaded := lossyWarnSeen.Load(key); loaded {
		return false
	}
	if lossyWarnCount.n >= lossyWarnCap {
		return false
	}
	lossyWarnSeen.Store(key, struct{}{})
	lossyWarnCount.n++
	return true
}

func fingerprint(combinator string, branches []any) (string, error) {
	encoded, err := json.Marshal(branches)
	if err != nil {
		return "", fmt.Errorf("fingerprint branches: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return combinator + ":" + hex.EncodeToString(sum[:]), nil
}

// unsupportedKeywords lists JSON Schema keywords that Kiro API rejects.
var unsupportedKeywords = map[string]struct{}{
	"additionalProperties":  {},
	"$schema":               {},
	"propertyNames":         {},
	"default":               {},
	"exclusiveMinimum":      {},
	"exclusiveMaximum":      {},
	"$defs":                 {},
	"$ref":                  {},
	"patternProperties":     {},
	"if":                    {},
	"then":                  {},
	"else":                  {},
	"dependentRequired":     {},
	"dependentSchemas":      {},
	"prefixItems":           {},
	"unevaluatedProperties": {},
	"unevaluatedItems":      {},
	"contentMediaType":      {},
	"contentEncoding":       {},
	"format":                {},
	"pattern":               {},
	"minLength":             {},
	"maxLength":             {},
	"minimum":               {},
	"maximum":               {},
	"minItems":              {},
	"maxItems":              {},
	"uniqueItems":           {},
	"multipleOf":            {},
	"not":                   {},
}

// SanitizeJSONSchema recursively removes fields that Kiro API rejects.
func SanitizeJSONSchema(schema map[string]any) map[string]any {
	if schema == nil {
		return map[string]any{}
	}

	result := make(map[string]any, len(schema))

	// First pass: process all non-combinator keys.
	for key, value := range schema {
		if _, drop := unsupportedKeywords[key]; drop {
			continue
		}
		switch key {
		case "const":
			result["enum"] = []any{value}
		case "required":
			if arr, ok := value.([]any); ok && len(arr) == 0 {
				continue
			}
			result[key] = value
		case "anyOf", "oneOf", "allOf":
			// Handled in second pass.
		default:
			switch v := value.(type) {
			case map[string]any:
				result[key] = SanitizeJSONSchema(v)
			case []any:
				sanitized := make([]any, len(v))
				for i, item := range v {
					if m, ok := item.(map[string]any); ok {
						sanitized[i] = SanitizeJSONSchema(m)
					} else {
						sanitized[i] = item
					}
				}
				result[key] = sanitized
			default:
				result[key] = value
			}
		}
	}

	// Second pass: apply combinators last so they deterministically override.
	for key, value := range schema {
		switch key {
		case "anyOf", "oneOf":
			if arr, ok := value.([]any); ok && len(arr) > 0 {
				if merged := flattenEnumBranches(arr); merged != nil {
					maps.Copy(result, merged)
				} else if nonNull := dropNullBranches(arr); len(nonNull) == 1 {
					if m, ok := nonNull[0].(map[string]any); ok {
						maps.Copy(result, SanitizeJSONSchema(m))
					}
				} else if first, ok := arr[0].(map[string]any); ok {
					warnLossyOnce(key, arr)
					maps.Copy(result, SanitizeJSONSchema(first))
				}
			}
		case "allOf":
			if arr, ok := value.([]any); ok {
				for _, item := range arr {
					if m, ok := item.(map[string]any); ok {
						maps.Copy(result, SanitizeJSONSchema(m))
					}
				}
			}
		}
	}

	return result
}

// EnsureObjectRoot wraps a sanitized schema in an object envelope if its root
// type is not "object". Call this on the final schema passed to Kiro, not during
// recursive sanitization of nested properties.
//
// Kiro/Bedrock rejects any tool whose inputSchema.json.type is not "object":
//
//	ValidationException: The value at toolConfig.tools.0.toolSpec.inputSchema.json.type
//	must be one of the following: object. reason: TOOL_SCHEMA_INVALID
//
// Anthropic's API has no such constraint, so clients (including Claude Code's
// built-in tools like WebSearch) may send schemas with type:"string" or no type
// at all. This wrapper satisfies the validation without altering semantics for
// the model.
func EnsureObjectRoot(schema map[string]any) map[string]any {
	if len(schema) == 0 {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	t, _ := schema["type"].(string)
	if t == "object" || t == "" {
		// Already an object, or no type declared — add the type rather than
		// wrapping. Either way `properties` must be present: Kiro rejects the
		// whole request, not just the offending tool, when a toolSpecification
		// carries an object schema without it.
		schema["type"] = "object"
		if _, ok := schema["properties"]; !ok {
			schema["properties"] = map[string]any{}
		}
		return schema
	}
	// Non-object type: wrap in an object envelope.
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"input": schema,
		},
	}
}

// dropNullBranches returns branches that are not {type: "null"}.
func dropNullBranches(branches []any) []any {
	var result []any
	for _, b := range branches {
		m, ok := b.(map[string]any)
		if !ok || m["type"] != "null" {
			result = append(result, b)
		}
	}
	return result
}

// flattenEnumBranches merges anyOf/oneOf branches when all branches have enum values.
// Each branch is sanitized exactly once and the sanitized result is reused for
// enum/type extraction, avoiding the double SanitizeJSONSchema call that the
// previous combinator pass performed per branch.
// Returns a merged schema with combined enum, or nil if not all branches are enum-based.
func flattenEnumBranches(branches []any) map[string]any {
	if len(branches) == 0 {
		return nil
	}
	var allEnums []any
	var typ string
	typConsistent := true
	for _, branch := range branches {
		m, ok := branch.(map[string]any)
		if !ok {
			return nil
		}
		sanitized := SanitizeJSONSchema(m)
		enumVal, hasEnum := sanitized["enum"]
		if !hasEnum {
			return nil
		}
		arr, ok := enumVal.([]any)
		if !ok {
			return nil
		}
		allEnums = append(allEnums, arr...)
		if t, ok := sanitized["type"].(string); ok {
			if typ == "" {
				typ = t
			} else if typ != t {
				typConsistent = false
			}
		} else {
			typConsistent = false
		}
	}
	merged := map[string]any{"enum": allEnums}
	if typ != "" && typConsistent {
		merged["type"] = typ
	}
	return merged
}
