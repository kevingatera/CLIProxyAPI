package usage

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// fingerprintPrefix marks a stored key identifier.
const fingerprintPrefix = "key:sha256:"

// fingerprintHexLen is the number of hex digits kept from the SHA-256 digest (64 bits).
const fingerprintHexLen = 16

// unattributedName groups requests that carried no client API key.
const unattributedName = "unattributed"

// authTypeOAuth is the auth kind upstream reports for OAuth accounts.
const authTypeOAuth = "oauth"

// KeyFingerprint returns the stored identifier for an API key. The digest is one-way
// and unsalted: it stops plain-text recovery, but short or guessable keys can be
// confirmed by hashing candidates.
func KeyFingerprint(rawKey string) string {
	sum := sha256.Sum256([]byte(rawKey))
	return fingerprintPrefix + hex.EncodeToString(sum[:fingerprintHexLen/2])
}

// apiName returns the aggregate key for a record. The type is known from the record
// field, not from the string: a client API key is always fingerprinted, and a request
// without one is grouped as unattributed.
func apiName(clientKey string) string {
	key := strings.TrimSpace(clientKey)
	if key == "" {
		return unattributedName
	}
	return KeyFingerprint(key)
}

// sourceName returns the stored source for a record. Only OAuth accounts keep their
// identity (typically the account email). Every other source is an upstream API key
// and is fingerprinted, whatever its characters.
func sourceName(authType, source string) string {
	source = strings.TrimSpace(source)
	if source == "" {
		return ""
	}
	if authType == authTypeOAuth {
		return source
	}
	return KeyFingerprint(source)
}

// isFingerprint reports whether name has exactly the stored fingerprint form.
func isFingerprint(name string) bool {
	digest, ok := strings.CutPrefix(name, fingerprintPrefix)
	if !ok || len(digest) != fingerprintHexLen {
		return false
	}
	for _, c := range digest {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// validAPIName reports whether name is a valid aggregate key for a snapshot.
func validAPIName(name string) bool {
	return name == unattributedName || isFingerprint(name)
}

// validSourceName reports whether a detail source is acceptable in a snapshot. API key
// sources must already be fingerprints. OAuth sources are kept as stored.
func validSourceName(authType, source string) bool {
	if source == "" || authType == authTypeOAuth {
		return true
	}
	return isFingerprint(source)
}
