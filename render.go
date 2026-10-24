package chok

import (
	"encoding/json/v2"
	"net/http"
)

// writeJSON writes v as the response body with code; an encoding
// failure becomes a logged 500 rather than a truncated body.
func writeJSON(w http.ResponseWriter, r *http.Request, code int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		ctx := r.Context()
		LoggerFrom(ctx).ErrorContext(ctx, "chok: response encoding failed", "error", err)
		code = http.StatusInternalServerError
		b, _ = json.Marshal(map[string]any{
			"code": code, "reason": ErrInternal.Reason, "message": ErrInternal.Message, "request_id": RequestID(ctx),
		})
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write(append(b, '\n'))
}
