package arcade

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
)

// decodeJSON refuses ignored fields and concatenated documents; every route
// uses the same contract. Content type is enforced at the production boundary.
func decodeJSON(r *http.Request, dst any) error {
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("request must contain exactly one JSON value")
	}
	return nil
}

func mutationPolicy(publicOrigin string, next http.Handler) http.Handler {
	p := http.NewCrossOriginProtection()
	if publicOrigin != "" {
		_ = p.AddTrustedOrigin(publicOrigin)
	}
	return p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" && r.Method != "HEAD" && r.Method != "OPTIONS" && r.Body != nil && r.ContentLength != 0 {
			media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || media != "application/json" {
				writeErr(w, 415, fmt.Errorf("mutation bodies require application/json"))
				return
			}
		}
		next.ServeHTTP(w, r)
	}))
}

type publicOriginKey struct{}

func withPublicOrigin(r *http.Request, origin string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), publicOriginKey{}, origin))
}
func secureRequest(r *http.Request) bool {
	origin, _ := r.Context().Value(publicOriginKey{}).(string)
	return r.TLS != nil || origin != ""
}
