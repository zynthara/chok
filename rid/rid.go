// Package rid generates resource identifiers.
//
// Format: {prefix}_{base62_id}
//
// Default length is 12 base62 chars → 3.2×10²¹ space.
// Prefix constraints: 1–10 chars, [a-z][a-z0-9]*, total RID length ≤ 23.
package rid

import (
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
)

const (
	alphabet     = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	defaultIDLen = 12
	maxRIDLen    = 23 // RID column size:24, reserve 1 byte
	maxPrefixLen = 10
	minPrefixLen = 1
)

// New generates a prefixed RID: "{prefix}_{12-char base62}".
// Panics if prefix is invalid.
func New(prefix string) string {
	validatePrefix(prefix, defaultIDLen)
	return prefix + "_" + randBase62(defaultIDLen)
}

// NewWithLength generates a prefixed RID with custom ID length.
// Panics if prefix is invalid or total length exceeds 23.
func NewWithLength(prefix string, n int) string {
	if n < 1 {
		panic("rid: id length must be >= 1")
	}
	validatePrefix(prefix, n)
	return prefix + "_" + randBase62(n)
}

// NewRaw generates a 12-char base62 string without prefix.
func NewRaw() string {
	return randBase62(defaultIDLen)
}

// Parse splits a RID into prefix and id parts.
// Returns error if the RID has no underscore separator.
func Parse(rid string) (prefix, id string, err error) {
	i := strings.IndexByte(rid, '_')
	if i < 0 {
		return "", "", errors.New("rid: invalid format, missing underscore separator")
	}
	return rid[:i], rid[i+1:], nil
}

// Prefix extracts the prefix from a RID.
// Returns empty string if no underscore is found.
func Prefix(rid string) string {
	i := strings.IndexByte(rid, '_')
	if i < 0 {
		return ""
	}
	return rid[:i]
}

// HasPrefix checks whether a RID starts with the given prefix followed by '_'.
func HasPrefix(rid, expect string) bool {
	if len(rid) < len(expect)+1 {
		return false
	}
	return rid[:len(expect)] == expect && rid[len(expect)] == '_'
}

// ValidatePrefix checks prefix constraints without generating an ID.
// idLen is the intended random part length (used for total-length check).
// Returns nil if valid, error describing the problem otherwise.
func ValidatePrefix(prefix string, idLen int) error {
	if len(prefix) < minPrefixLen || len(prefix) > maxPrefixLen {
		return fmt.Errorf("rid: prefix length must be %d–%d, got %d (%q)",
			minPrefixLen, maxPrefixLen, len(prefix), prefix)
	}
	// First char: [a-z]
	if prefix[0] < 'a' || prefix[0] > 'z' {
		return fmt.Errorf("rid: prefix must start with [a-z], got %q", prefix)
	}
	// Remaining chars: [a-z0-9]
	for i := 1; i < len(prefix); i++ {
		c := prefix[i]
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')) {
			return fmt.Errorf("rid: prefix char %d must be [a-z0-9], got %q in %q", i, string(c), prefix)
		}
	}
	// Total length: prefix + "_" + id <= 23
	total := len(prefix) + 1 + idLen
	if total > maxRIDLen {
		return fmt.Errorf("rid: total length %d exceeds max %d (prefix %q + 1 + id %d)",
			total, maxRIDLen, prefix, idLen)
	}
	return nil
}

// validatePrefix panics if prefix is invalid.
func validatePrefix(prefix string, idLen int) {
	if err := ValidatePrefix(prefix, idLen); err != nil {
		panic(err.Error())
	}
}

// ValidateRID checks whether a full RID conforms to the schema:
//   - total length 1–maxRIDLen
//   - shape either "id" (no prefix) or "prefix_id" where prefix is valid
//     per ValidatePrefix and id is at least one character of [A-Za-z0-9]
//
// Used by callers (e.g. db.Model.BeforeCreate) to reject malformed
// user-supplied RIDs before they reach the database.
func ValidateRID(s string) error {
	if len(s) < 1 {
		return errors.New("rid: empty")
	}
	if len(s) > maxRIDLen {
		return fmt.Errorf("rid: length %d exceeds max %d", len(s), maxRIDLen)
	}
	idx := strings.IndexByte(s, '_')
	if idx < 0 {
		// No prefix form: every byte must be [A-Za-z0-9]
		return validateIDChars(s)
	}
	prefix := s[:idx]
	id := s[idx+1:]
	if len(id) == 0 {
		return errors.New("rid: empty id part")
	}
	if err := ValidatePrefix(prefix, len(id)); err != nil {
		return err
	}
	return validateIDChars(id)
}

func validateIDChars(s string) error {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')) {
			return fmt.Errorf("rid: char %d must be [A-Za-z0-9], got %q", i, string(c))
		}
	}
	return nil
}

// maxUnbiased is the largest multiple of 62 that fits in a byte (62*4=248).
// Bytes >= 248 are discarded to eliminate modulo bias.
const maxUnbiased = 248

func randBase62(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("rid: crypto/rand failed: " + err.Error())
	}
	for i := range b {
		for b[i] >= maxUnbiased {
			var tmp [1]byte
			if _, err := rand.Read(tmp[:]); err != nil {
				panic("rid: crypto/rand failed: " + err.Error())
			}
			b[i] = tmp[0]
		}
		b[i] = alphabet[b[i]%62]
	}
	return string(b)
}
