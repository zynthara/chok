package auth

import (
	"errors"

	"golang.org/x/crypto/bcrypt"
)

// DefaultPasswordCost is the bcrypt cost chok uses for HashPassword. 12
// is a reasonable 2024 default — roughly 250ms on modern x86 per hash,
// slow enough to frustrate brute force, fast enough to serve login
// traffic. bcrypt.DefaultCost (10) is below the current OWASP
// recommendation. Tests that feel slow can override via HashPasswordCost.
const DefaultPasswordCost = 12

// HashPassword returns a bcrypt hash of the password using the default
// cost (DefaultPasswordCost). Returns an error if password exceeds
// bcrypt's 72-byte input limit, since bcrypt silently truncates longer
// inputs.
func HashPassword(password string) (string, error) {
	return HashPasswordCost(password, DefaultPasswordCost)
}

// HashPasswordCost hashes password at the given bcrypt cost. Use this
// escape hatch when a specific deployment requires a different cost
// (lower for tests that run HashPassword in tight loops, higher for
// paranoid deployments). cost is clamped to bcrypt.MinCost..MaxCost.
func HashPasswordCost(password string, cost int) (string, error) {
	if len(password) > 72 {
		return "", errors.New("auth: password exceeds bcrypt 72-byte limit")
	}
	if cost < bcrypt.MinCost {
		cost = bcrypt.MinCost
	}
	if cost > bcrypt.MaxCost {
		cost = bcrypt.MaxCost
	}
	bytes, err := bcrypt.GenerateFromPassword([]byte(password), cost)
	return string(bytes), err
}

// ComparePassword compares a bcrypt hashed password with a plaintext candidate.
// Returns nil on match, error on mismatch.
func ComparePassword(hashed, plain string) error {
	return bcrypt.CompareHashAndPassword([]byte(hashed), []byte(plain))
}
