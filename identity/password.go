package identity

import (
	"errors"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"github.com/zynthara/chok"
	"github.com/zynthara/chok/internal/apptest"
)

// passwordCost is the bcrypt cost of every password hash: about 250ms on
// a current core, slow for a brute force, fast enough for logins.
// bcrypt.DefaultCost (10) is below the OWASP recommendation.
const passwordCost = 12

// costOf is the cost a new Identity hashes with: passwordCost, unless a
// test binary lowered it through choktest.
func costOf() int {
	if c := apptest.PasswordCost.Load(); c > 0 {
		return int(c)
	}
	return passwordCost
}

var errPasswordTooLong = errors.New("identity: a password longer than 72 bytes cannot be hashed (bcrypt would truncate it)")

// hashPassword hashes password at cost, clamped to bcrypt's range. A
// password over bcrypt's 72-byte input is refused, not truncated.
func hashPassword(password string, cost int) (string, error) {
	if len(password) > 72 {
		return "", errPasswordTooLong
	}
	cost = min(max(cost, bcrypt.MinCost), bcrypt.MaxCost)
	b, err := bcrypt.GenerateFromPassword([]byte(password), cost)
	return string(b), err
}

// comparePassword returns nil when plain matches hash.
func comparePassword(hash, plain string) error {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain))
}

// checkPasswordStrength refuses the weakest shapes — all digits, all one
// case — on top of the 8-to-72 length the request validates: a password
// mixes at least two of lower case, upper case, digits and symbols.
func checkPasswordStrength(field, pwd string) error {
	var lower, upper, digit, other bool
	for _, r := range pwd {
		switch {
		case r >= 'a' && r <= 'z':
			lower = true
		case r >= 'A' && r <= 'Z':
			upper = true
		case r >= '0' && r <= '9':
			digit = true
		default:
			other = true
		}
	}
	classes := 0
	for _, b := range []bool{lower, upper, digit, other} {
		if b {
			classes++
		}
	}
	if classes < 2 {
		return chok.ErrValidation.
			Msgf("%s must mix at least two of: lower case, upper case, digits, symbols", field).
			With("fields", map[string]string{field: "strength"})
	}
	return nil
}

// normalizeEmail is the one spelling of an address for lookups, storage
// and the login throttle: trimmed and lower-cased, so "Alice@Example.com "
// is neither a second account nor a second rate-limit bucket.
func normalizeEmail(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// maskEmail makes a display name from an address when none is given:
// the local part and the domain labels masked, the top-level suffix kept.
//
//	alice@test.com        → a***e@t**t.com
//	john.doe@example.com  → j******e@e*****e.com
//	alice@mail.test.co.jp → a***e@m**l.t**t.co.jp
func maskEmail(email string) string {
	at := strings.LastIndex(email, "@")
	if at <= 0 {
		return email
	}
	return maskPart(email[:at]) + "@" + maskDomain(email[at+1:])
}

func maskPart(s string) string {
	switch len(s) {
	case 0:
		return "*"
	case 1:
		return s + "*"
	case 2:
		return s[:1] + "*" + s[1:]
	default:
		return s[:1] + strings.Repeat("*", len(s)-2) + s[len(s)-1:]
	}
}

// maskDomain masks the domain labels and keeps the suffix: the trailing
// labels of three characters or fewer (com, co.jp), at least the last.
func maskDomain(domain string) string {
	parts := strings.Split(domain, ".")
	if len(parts) <= 1 {
		return maskPart(domain)
	}
	suffix := len(parts)
	for i := len(parts) - 1; i >= 1; i-- {
		if len(parts[i]) > 3 {
			break
		}
		suffix = i
	}
	if suffix >= len(parts) {
		suffix = len(parts) - 1
	}
	var b strings.Builder
	for i, p := range parts {
		if i > 0 {
			b.WriteByte('.')
		}
		if i < suffix {
			b.WriteString(maskPart(p))
		} else {
			b.WriteString(p)
		}
	}
	return b.String()
}
