package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"testing"
)

func TestClassifyError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want failureKind
	}{
		{"redirect", errRedirect, kindRedirect},
		{"wrapped invalid payload", fmt.Errorf("probe: %w", errInvalidPayload), kindInvalidPayload},
		{"timeout", context.DeadlineExceeded, kindTimeout},
		{"dns", &net.DNSError{Err: "no such host", Name: "backend.invalid"}, kindDNS},
		{"connection refused", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}, kindConnectionRefused},
		{"other network op", &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset")}, failureKind("network_read")},
		{"json syntax", &json.SyntaxError{Offset: 1}, kindInvalidJSON},
		{"unknown", errors.New("boom"), failureKind("*errors.errorString")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyError(tc.err); got != tc.want {
				t.Fatalf("classifyError() = %q, want %q", got, tc.want)
			}
		})
	}
}
