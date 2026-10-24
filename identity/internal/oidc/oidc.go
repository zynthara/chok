// Package oidc is the OpenID Connect a provider needs and no more: the
// issuer's discovery document, and the verification of its ID tokens
// against the keys the issuer publishes. It stands in for a full OIDC
// library: identity's providers use one flow, on two issuers.
package oidc

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Metadata is the part of an issuer's discovery document the sign-in
// uses.
type Metadata struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
}

// Issuer is one OpenID Connect issuer. It discovers its endpoints on
// first use and caches the signing keys, fetching them again when a token
// names a key it does not know — the issuer rotated — at most once a
// minute.
type Issuer struct {
	url    string
	client *http.Client
	now    func() time.Time

	mu      sync.Mutex
	meta    *Metadata
	keys    map[string]crypto.PublicKey // by kid
	fetched time.Time
}

// refetchAfter is how soon an unknown key id may fetch the keys again,
// so tokens with made-up ids cannot hammer the issuer.
const refetchAfter = time.Minute

// leeway tolerates clock drift between the issuer and this server.
const leeway = time.Minute

// maxDocument caps the discovery document and the key set.
const maxDocument = 1 << 20

// New returns the issuer at url (https://accounts.google.com); nothing is
// fetched until it is needed.
func New(url string) *Issuer {
	return &Issuer{url: strings.TrimRight(url, "/"), client: &http.Client{Timeout: 10 * time.Second}, now: time.Now}
}

// Metadata returns the issuer's discovery document, fetched once; a
// failed fetch is tried again on the next call.
func (i *Issuer) Metadata(ctx context.Context) (*Metadata, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.meta != nil {
		return i.meta, nil
	}
	var m Metadata
	if err := i.get(ctx, i.url+"/.well-known/openid-configuration", &m); err != nil {
		return nil, fmt.Errorf("oidc: discovery on %s: %w", i.url, err)
	}
	// The document must speak for the issuer it was fetched from.
	if strings.TrimRight(m.Issuer, "/") != i.url {
		return nil, fmt.Errorf("oidc: %s describes the issuer %q", i.url, m.Issuer)
	}
	if m.AuthorizationEndpoint == "" || m.TokenEndpoint == "" || m.JWKSURI == "" {
		return nil, fmt.Errorf("oidc: %s's discovery document lacks an endpoint", i.url)
	}
	i.meta = &m
	return i.meta, nil
}

// signingMethods are the algorithms an ID token may use: the asymmetric
// ones. HMAC is refused, so a public key can never be used as a secret.
var signingMethods = []string{"RS256", "RS384", "RS512", "ES256", "ES384", "ES512"}

// Verify checks an ID token — its signature against the issuer's keys,
// iss, aud holding clientID, exp (required) and iat, with a minute of
// leeway — and decodes its claims into out.
func (i *Issuer) Verify(ctx context.Context, raw, clientID string, out any) error {
	meta, err := i.Metadata(ctx)
	if err != nil {
		return err
	}
	parser := jwt.NewParser(
		jwt.WithValidMethods(signingMethods), jwt.WithIssuer(meta.Issuer), jwt.WithAudience(clientID),
		jwt.WithExpirationRequired(), jwt.WithIssuedAt(), jwt.WithLeeway(leeway), jwt.WithTimeFunc(i.now),
	)
	claims := jwt.MapClaims{}
	tok, err := parser.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		return i.key(ctx, meta.JWKSURI, kid)
	})
	if err != nil || !tok.Valid {
		return fmt.Errorf("oidc: id_token: %w", err)
	}
	b, err := json.Marshal(claims)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// key returns the issuer's key kid, fetching the key set when it is not
// cached and the last fetch is a minute old.
func (i *Issuer) key(ctx context.Context, jwksURI, kid string) (crypto.PublicKey, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if k, ok := i.lookup(kid); ok {
		return k, nil
	}
	if i.keys != nil && i.now().Sub(i.fetched) < refetchAfter {
		return nil, fmt.Errorf("oidc: no key %q", kid)
	}
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := i.get(ctx, jwksURI, &set); err != nil {
		return nil, fmt.Errorf("oidc: fetch the keys: %w", err)
	}
	keys := map[string]crypto.PublicKey{}
	for _, k := range set.Keys {
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		if pub, err := k.public(); err == nil {
			keys[k.Kid] = pub
		}
	}
	i.keys, i.fetched = keys, i.now()
	if k, ok := i.lookup(kid); ok {
		return k, nil
	}
	return nil, fmt.Errorf("oidc: no key %q", kid)
}

// lookup finds kid; a token without kid takes the only key there is.
func (i *Issuer) lookup(kid string) (crypto.PublicKey, bool) {
	if kid == "" && len(i.keys) == 1 {
		for _, k := range i.keys {
			return k, true
		}
	}
	k, ok := i.keys[kid]
	return k, ok
}

func (i *Issuer) get(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := i.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxDocument)).Decode(out)
}

// jwk is one JSON Web Key (RFC 7517) of an RSA or EC signing key.
type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

var errKey = errors.New("oidc: unusable key")

func (k jwk) public() (crypto.PublicKey, error) {
	switch k.Kty {
	case "RSA":
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil || len(n) == 0 || len(e) == 0 || len(e) > 4 {
			return nil, errKey
		}
		exp := new(big.Int).SetBytes(e)
		return &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(exp.Int64())}, nil
	case "EC":
		var curve elliptic.Curve
		switch k.Crv {
		case "P-256":
			curve = elliptic.P256()
		case "P-384":
			curve = elliptic.P384()
		case "P-521":
			curve = elliptic.P521()
		default:
			return nil, errKey
		}
		x, err1 := base64.RawURLEncoding.DecodeString(k.X)
		y, err2 := base64.RawURLEncoding.DecodeString(k.Y)
		size := (curve.Params().BitSize + 7) / 8
		if err1 != nil || err2 != nil || len(x) != size || len(y) != size {
			return nil, errKey
		}
		point := append(append([]byte{4}, x...), y...)
		return ecdsa.ParseUncompressedPublicKey(curve, point)
	}
	return nil, errKey
}
