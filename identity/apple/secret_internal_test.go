package apple

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"testing"
	"time"
)

func TestClientSecret_RenewsBeforeExpiry(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	c := &clientSecret{teamID: "TEAM123456", keyID: "KEY1234567", clientID: "com.example.web", key: key, now: func() time.Time { return now }}
	first, err := c.get()
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(secretTTL - 2*time.Minute)
	if again, _ := c.get(); again != first {
		t.Fatal("reused while more than a minute is left")
	}
	now = now.Add(90 * time.Second) // under a minute left
	if renewed, _ := c.get(); renewed == first {
		t.Fatal("renewed a minute before expiry")
	}
}
