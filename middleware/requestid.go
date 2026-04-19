package middleware

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zynthara/chok/internal/ctxval"
)

// fallbackCounter is bumped on every generateID call that falls back to
// the deterministic path, guaranteeing distinct IDs even when many
// requests hit the fallback in the same nanosecond.
var fallbackCounter uint64

const headerXRequestID = "X-Request-ID"

// maxRequestIDLen is the maximum accepted length for a client-supplied
// X-Request-ID to prevent memory waste and log injection with oversized values.
const maxRequestIDLen = 128

// RequestID generates or propagates a request ID via the X-Request-ID header.
// Client-supplied IDs are truncated to 128 chars and reduced to a strict
// ASCII safe-set ([A-Za-z0-9._-]) before propagation. The whitelist
// approach blocks log-injection vectors that the previous "strip < 0x20"
// filter missed: high-byte UTF-8 sequences such as U+2028 (LINE
// SEPARATOR) / U+2029 (PARAGRAPH SEPARATOR) used to be passed through
// verbatim and could split log lines in viewers that interpret them as
// breaks.
//
// As a convenience, the resolved client IP (c.ClientIP()) is also stored
// in the request context so downstream handlers — in particular the
// account module's login rate limiter — can key on source address
// without needing direct access to *gin.Context.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		rid := c.GetHeader(headerXRequestID)
		if rid == "" {
			rid = generateID()
		} else {
			rid = sanitizeRequestID(rid)
		}
		ctx := ctxval.WithRequestID(c.Request.Context(), rid)
		if ip := c.ClientIP(); ip != "" {
			ctx = ctxval.WithClientIP(ctx, ip)
		}
		c.Request = c.Request.WithContext(ctx)
		c.Header(headerXRequestID, rid)
		c.Next()
	}
}

// sanitizeRequestID truncates and reduces a client-supplied request ID
// to a safe ASCII subset. Bytes outside [A-Za-z0-9._-] are dropped; if
// nothing remains, a fresh random ID is generated. Truncation runs
// before filtering so an oversized header can't bypass the byte-budget
// by being mostly invalid characters.
func sanitizeRequestID(id string) string {
	if len(id) > maxRequestIDLen {
		id = id[:maxRequestIDLen]
	}
	clean := make([]byte, 0, len(id))
	for i := range len(id) {
		c := id[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c == '.', c == '_', c == '-':
		default:
			continue
		}
		clean = append(clean, c)
	}
	if len(clean) == 0 {
		return generateID()
	}
	return string(clean)
}

func generateID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand can fail in very constrained environments (getrandom
		// returning EAGAIN before the entropy pool is seeded, for
		// example). Fall back to a timestamp+counter composite that's
		// still unique per process and per call, so request logs can
		// correlate even when the primary randomness source is unavailable.
		// The returned ID is NOT cryptographically random, but request IDs
		// don't need to be — they're correlation keys, not secrets.
		binary.BigEndian.PutUint64(b[:8], uint64(time.Now().UnixNano()))
		binary.BigEndian.PutUint64(b[8:], atomic.AddUint64(&fallbackCounter, 1))
	}
	return hex.EncodeToString(b)
}
