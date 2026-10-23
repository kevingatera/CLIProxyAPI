package usage

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// fingerprintPrefix marks a persisted key identifier. The digest is one-way,
// so a snapshot never holds a raw API key.
const fingerprintPrefix = "key:sha256:"

// fingerprintHexLen is the number of hex digits kept from the SHA-256 digest.
const fingerprintHexLen = 16

// KeyFingerprint returns the stable, non-secret identifier stored in place of an API key.
func KeyFingerprint(rawKey string) string {
	sum := sha256.Sum256([]byte(rawKey))
	return fingerprintPrefix + hex.EncodeToString(sum[:fingerprintHexLen/2])
}

func isFingerprint(value string) bool {
	digest, ok := strings.CutPrefix(value, fingerprintPrefix)
	if !ok || len(digest) != fingerprintHexLen {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}

// isRouteLabel reports whether value is a route label such as "POST /v1/responses".
// Route labels are not secrets; API keys never contain whitespace or a leading slash.
func isRouteLabel(value string) bool {
	return strings.HasPrefix(value, "/") || strings.ContainsAny(value, " \t") || value == "unknown"
}

// normalizeAPIName returns the persisted form of an aggregation key.
func normalizeAPIName(name string) string {
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		return "unknown"
	case isFingerprint(name), isRouteLabel(name):
		return name
	default:
		return KeyFingerprint(name)
	}
}

// normalizeSource keeps account emails and fingerprints everything else. For api_key
// auths the source is the upstream API key itself, so only emails pass through.
func normalizeSource(source string) string {
	source = strings.TrimSpace(source)
	if source == "" || isFingerprint(source) || strings.Contains(source, "@") {
		return source
	}
	return KeyFingerprint(source)
}
