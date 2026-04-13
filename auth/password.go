package auth

import "golang.org/x/crypto/bcrypt"

// HashPassword returns a bcrypt hash of the password.
func HashPassword(password string) (string, error) {
	bytes, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(bytes), err
}

// ComparePassword compares a bcrypt hashed password with a plaintext candidate.
// Returns nil on match, error on mismatch.
func ComparePassword(hashed, plain string) error {
	return bcrypt.CompareHashAndPassword([]byte(hashed), []byte(plain))
}
