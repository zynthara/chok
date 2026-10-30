package identity

import (
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/zynthara/chok"
	"github.com/zynthara/chok/internal/apptest"
)

func TestPassword_HashAndCompare(t *testing.T) {
	hash, err := hashPassword("password123", bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if comparePassword(hash, "password123") != nil || comparePassword(hash, "wrong") == nil {
		t.Fatal("compare matches the password and nothing else")
	}
	if cost, _ := bcrypt.Cost([]byte(hash)); cost != bcrypt.MinCost {
		t.Fatalf("cost = %d", cost)
	}
	// Out-of-range costs are clamped, not refused.
	if h, err := hashPassword("password123", 1); err != nil || mustCost(t, h) != bcrypt.MinCost {
		t.Fatalf("a cost below the minimum: %v", err)
	}
	// Over 72 bytes bcrypt would truncate: refused instead.
	if _, err := hashPassword(strings.Repeat("a", 73), bcrypt.MinCost); !errors.Is(err, errPasswordTooLong) {
		t.Fatalf("73 bytes: %v", err)
	}
}

func mustCost(t *testing.T, hash string) int {
	t.Helper()
	c, err := bcrypt.Cost([]byte(hash))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestPassword_CostFollowsTheTestHook(t *testing.T) {
	saved := apptest.PasswordCost.Load()
	t.Cleanup(func() { apptest.PasswordCost.Store(saved) })
	apptest.PasswordCost.Store(0)
	if costOf() != passwordCost || passwordCost != 12 {
		t.Fatalf("the production cost is 12, got %d", costOf())
	}
	apptest.PasswordCost.Store(5)
	if costOf() != 5 {
		t.Fatalf("a test binary lowers it: %d", costOf())
	}
}

func TestPassword_Strength(t *testing.T) {
	for _, weak := range []string{"aaaaaaaa", "AAAAAAAA", "12345678", "!!!!!!!!"} {
		if err := checkPasswordStrength("password", weak); !errors.Is(err, chok.ErrValidation) {
			t.Errorf("%q passed", weak)
		}
	}
	for _, strong := range []string{"password123", "PASSword", "pass word", "Пароль123"} {
		if err := checkPasswordStrength("password", strong); err != nil {
			t.Errorf("%q: %v", strong, err)
		}
	}
}

func TestMaskEmail(t *testing.T) {
	for email, want := range map[string]string{
		"a@test.com":            "a*@t**t.com",
		"ab@test.com":           "a*b@t**t.com",
		"alice@test.com":        "a***e@t**t.com",
		"john.doe@example.com":  "j******e@e*****e.com",
		"alice@mail.test.co.jp": "a***e@m**l.t**t.co.jp",
		"bob@qq.com":            "b*b@q*q.com",
		"no-at-sign":            "no-at-sign",
	} {
		if got := maskEmail(email); got != want {
			t.Errorf("maskEmail(%q) = %q, want %q", email, got, want)
		}
	}
}
