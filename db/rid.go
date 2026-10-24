package db

import (
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
)

// A RID is a model's public identifier: an optional lower-case prefix,
// an underscore, and 12 base62 characters (pst_3fK9aQ2mZx7B). It fits the
// rid column's 24 bytes; the numeric primary key never leaves the server.

const (
	ridAlphabet  = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	ridIDLen     = 12
	ridMaxLen    = 23
	ridMaxPrefix = 10
)

// NewRID returns a fresh RID with prefix. It panics on a prefix that is
// not 1–10 characters of [a-z0-9] starting with a letter, which is a
// programming error.
func NewRID(prefix string) string {
	if err := validatePrefix(prefix); err != nil {
		panic(err.Error())
	}
	return prefix + "_" + randBase62(ridIDLen)
}

// ParseRID returns the prefix of rid and whether rid is well formed. A
// RID without an underscore is well formed and has no prefix.
func ParseRID(rid string) (prefix string, ok bool) {
	if validateRID(rid) != nil {
		return "", false
	}
	if before, _, ok := strings.Cut(rid, "_"); ok {
		return before, true
	}
	return "", true
}

func validatePrefix(prefix string) error {
	if len(prefix) < 1 || len(prefix) > ridMaxPrefix {
		return fmt.Errorf("db: RID prefix must be 1–%d characters, got %q", ridMaxPrefix, prefix)
	}
	if prefix[0] < 'a' || prefix[0] > 'z' {
		return fmt.Errorf("db: RID prefix must start with a-z, got %q", prefix)
	}
	for i := 1; i < len(prefix); i++ {
		if c := prefix[i]; (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return fmt.Errorf("db: RID prefix may hold only a-z and 0-9, got %q", prefix)
		}
	}
	return nil
}

func validateRID(s string) error {
	if s == "" {
		return errors.New("db: empty RID")
	}
	if len(s) > ridMaxLen {
		return fmt.Errorf("db: RID longer than %d bytes", ridMaxLen)
	}
	id := s
	if before, after, ok := strings.Cut(s, "_"); ok {
		if err := validatePrefix(before); err != nil {
			return err
		}
		id = after
		if id == "" {
			return errors.New("db: RID has no id after its prefix")
		}
	}
	for i := 0; i < len(id); i++ {
		if c := id[i]; (c < '0' || c > '9') && (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') {
			return fmt.Errorf("db: RID id may hold only A-Z, a-z and 0-9, got %q", s)
		}
	}
	return nil
}

// randBase62 draws n characters without modulo bias.
func randBase62(n int) string {
	const unbiased = 248 // the largest multiple of 62 below 256
	b := make([]byte, n)
	_, _ = rand.Read(b) // crypto/rand.Read never fails
	for i := range b {
		for b[i] >= unbiased {
			var one [1]byte
			_, _ = rand.Read(one[:])
			b[i] = one[0]
		}
		b[i] = ridAlphabet[b[i]%62]
	}
	return string(b)
}
