package identity

import (
	"crypto/subtle"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"

	"github.com/zynthara/chok/v3"
)

// maxJSONBody caps the bodies of the raw OAuth routes, which take a code
// or a provider name.
const maxJSONBody = 1 << 20

// readJSON decodes the body of a route that sets cookies, and so cannot
// be a typed chok handler, with the same strictness: unknown members and
// malformed JSON are 400s.
func readJSON(w http.ResponseWriter, r *http.Request, out any) error {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxJSONBody))
	if err != nil {
		if _, tooBig := errors.AsType[*http.MaxBytesError](err); tooBig {
			return chok.NewError(http.StatusRequestEntityTooLarge, "PayloadTooLarge", "the request body is too large")
		}
		return chok.ErrBadRequest.Msgf("read the request body").Wrap(err)
	}
	if len(data) == 0 {
		return chok.ErrBadRequest.Msgf("a JSON body is required")
	}
	if err := json.Unmarshal(data, out, json.RejectUnknownMembers(true)); err != nil {
		return chok.ErrBadRequest.Msgf("invalid JSON body").Wrap(err)
	}
	return nil
}

// writeJSON answers v as JSON.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.MarshalWrite(w, v)
}

// hmacEqualHex compares a digest with a hex-encoded one in constant time.
func hmacEqualHex(got []byte, wantHex string) bool {
	want, err := hex.DecodeString(wantHex)
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}
