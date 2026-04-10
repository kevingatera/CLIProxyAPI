package auth

import (
	"net/http"
	"testing"
)

func TestErrorStatusCodeFromExplicitHTTPStatus(t *testing.T) {
	err := &Error{Code: "auth_not_found", HTTPStatus: http.StatusUnauthorized}
	if got := err.StatusCode(); got != http.StatusUnauthorized {
		t.Fatalf("StatusCode() = %d, want %d", got, http.StatusUnauthorized)
	}
}

func TestErrorStatusCodeFromCodeFallback(t *testing.T) {
	tests := []struct {
		name string
		code string
		want int
	}{
		{name: "provider missing", code: "provider_not_found", want: http.StatusBadRequest},
		{name: "auth missing", code: "auth_not_found", want: http.StatusServiceUnavailable},
		{name: "auth unavailable", code: "auth_unavailable", want: http.StatusServiceUnavailable},
		{name: "executor missing", code: "executor_not_found", want: http.StatusServiceUnavailable},
		{name: "invalid request", code: "invalid_request", want: http.StatusBadRequest},
		{name: "not supported", code: "not_supported", want: http.StatusNotImplemented},
		{name: "unknown code", code: "unknown", want: 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := &Error{Code: tc.code}
			if got := err.StatusCode(); got != tc.want {
				t.Fatalf("StatusCode() = %d, want %d", got, tc.want)
			}
		})
	}
}
