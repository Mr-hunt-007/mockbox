// Package httpx holds small HTTP helpers shared by the mockbox backends.
package httpx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/Mr-hunt-007/mockbox/internal/jsonx"
)

// MaxBodyBytes caps request bodies.
const MaxBodyBytes = 10 << 20

// Error is an HTTP error with a status code and a human readable message.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return e.Message }

// Errorf builds an *Error.
func Errorf(status int, format string, args ...any) *Error {
	return &Error{Status: status, Message: fmt.Sprintf(format, args...)}
}

// WriteJSON writes v as indented JSON with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	body := jsonx.Marshal(v, "  ")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
	_, _ = w.Write([]byte("\n"))
}

// WriteError writes the documented error shape:
// {"error": "<status text>", "status": <code>, "message": "<detail>"}.
func WriteError(w http.ResponseWriter, status int, message string) {
	o := jsonx.NewObject()
	o.Set("error", http.StatusText(status))
	o.Set("status", status)
	o.Set("message", message)
	WriteJSON(w, status, o)
}

// WriteErr writes an *Error (or a 500 for any other error).
func WriteErr(w http.ResponseWriter, err error) {
	var he *Error
	if errors.As(err, &he) {
		WriteError(w, he.Status, he.Message)
		return
	}
	WriteError(w, http.StatusInternalServerError, err.Error())
}

// MethodNotAllowed writes a 405 with an Allow header.
func MethodNotAllowed(w http.ResponseWriter, r *http.Request, allow []string) {
	w.Header().Set("Allow", strings.Join(allow, ", "))
	WriteError(w, http.StatusMethodNotAllowed, fmt.Sprintf("%s is not allowed on %s (allowed: %s)", r.Method, r.URL.Path, strings.Join(allow, ", ")))
}

// NotFound writes the JSON 404 for an unknown route.
func NotFound(w http.ResponseWriter, r *http.Request) {
	WriteError(w, http.StatusNotFound, fmt.Sprintf("no route for %s %s", r.Method, r.URL.Path))
}

// ReadJSON reads and parses the request body. An empty body is an error.
func ReadJSON(w http.ResponseWriter, r *http.Request) (any, *Error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, Errorf(http.StatusRequestEntityTooLarge, "request body is larger than %d bytes", MaxBodyBytes)
		}
		return nil, Errorf(http.StatusBadRequest, "cannot read request body: %v", err)
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil, Errorf(http.StatusBadRequest, "request body is empty, expected JSON")
	}
	v, err := jsonx.Parse(body)
	if err != nil {
		return nil, Errorf(http.StatusBadRequest, "invalid JSON body: %v", err)
	}
	return v, nil
}

// CheckJSON reads the body and reports invalid JSON. An empty body is fine.
func CheckJSON(w http.ResponseWriter, r *http.Request) *Error {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		return Errorf(http.StatusBadRequest, "cannot read request body: %v", err)
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil
	}
	if _, err := jsonx.Parse(body); err != nil {
		return Errorf(http.StatusBadRequest, "invalid JSON body: %v", err)
	}
	return nil
}

// ReadJSONObject reads the body and requires a JSON object.
func ReadJSONObject(w http.ResponseWriter, r *http.Request) (*jsonx.Object, *Error) {
	v, herr := ReadJSON(w, r)
	if herr != nil {
		return nil, herr
	}
	o, ok := v.(*jsonx.Object)
	if !ok {
		return nil, Errorf(http.StatusBadRequest, "request body must be a JSON object, got %s", TypeName(v))
	}
	return o, nil
}

// TypeName names a JSON value's type for error messages.
func TypeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "a boolean"
	case string:
		return "a string"
	case []any:
		return "an array"
	case *jsonx.Object:
		return "an object"
	default:
		return "a number"
	}
}

type ctxKey int

const origURLKey ctxKey = 1

// WithOriginalURL records the URL the client requested before any rewrite.
func WithOriginalURL(r *http.Request, u *url.URL) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), origURLKey, u))
}

// OriginalURL returns the URL the client requested before any rewrite.
func OriginalURL(r *http.Request) *url.URL {
	if u, ok := r.Context().Value(origURLKey).(*url.URL); ok {
		return u
	}
	return r.URL
}

// PrefersJSON reports whether the Accept header ranks application/json above
// text/html. Ties (including */* alone) go to HTML.
func PrefersJSON(accept string) bool {
	if strings.TrimSpace(accept) == "" {
		return false
	}
	jsonQ, htmlQ := -1.0, -1.0
	jsonSpec, htmlSpec := 0, 0 // specificity of the range that set the q value
	for _, part := range strings.Split(accept, ",") {
		fields := strings.Split(part, ";")
		mt := strings.ToLower(strings.TrimSpace(fields[0]))
		q := 1.0
		for _, p := range fields[1:] {
			p = strings.TrimSpace(p)
			if strings.HasPrefix(p, "q=") {
				if f, err := strconv.ParseFloat(p[2:], 64); err == nil {
					q = f
				}
			}
		}
		set := func(cur *float64, spec *int, s int) {
			if s > *spec {
				*cur, *spec = q, s
			}
		}
		switch mt {
		case "application/json":
			set(&jsonQ, &jsonSpec, 3)
		case "application/*":
			set(&jsonQ, &jsonSpec, 2)
		case "text/html":
			set(&htmlQ, &htmlSpec, 3)
		case "text/*":
			set(&htmlQ, &htmlSpec, 2)
		case "*/*":
			set(&jsonQ, &jsonSpec, 1)
			set(&htmlQ, &htmlSpec, 1)
		}
	}
	return jsonQ > 0 && jsonQ > htmlQ
}
