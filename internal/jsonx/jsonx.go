// Package jsonx holds the encoding/json/v2 options every chok body uses.
//
// json/v2 has no default representation for time.Duration; chok writes
// one as its String form ("1h30m0s"), the way configuration spells it,
// and reads either that string or an integer count of nanoseconds.
package jsonx

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"time"
)

// Options apply to every request and response body.
var Options = json.JoinOptions(
	json.WithMarshalers(json.MarshalToFunc(func(enc *jsontext.Encoder, d time.Duration) error {
		return enc.WriteToken(jsontext.String(d.String()))
	})),
	json.WithUnmarshalers(json.UnmarshalFromFunc(func(dec *jsontext.Decoder, d *time.Duration) error {
		tok, err := dec.ReadToken()
		if err != nil {
			return err
		}
		switch tok.Kind() {
		case '"':
			v, err := time.ParseDuration(tok.String())
			if err != nil {
				return err
			}
			*d = v
		case '0':
			n, err := tok.Int()
			if err != nil {
				return err
			}
			*d = time.Duration(n)
		default:
			return errDuration
		}
		return nil
	})),
)

var errDuration = errors.New(`a duration is a string such as "1h30m" or a number of nanoseconds`)
