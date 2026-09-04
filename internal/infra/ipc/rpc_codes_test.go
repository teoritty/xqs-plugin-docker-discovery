package ipc

import (
	"errors"
	"fmt"
	"testing"
)

// TestCodeHelpersSeeThroughWrapping pins the property the type assertion these helpers used to make
// did not have.
//
// Nothing in this plugin hands the host's error back unwrapped — chanbus adds context to every one
// of them — so an unwrapped case alone would have passed against the broken version and told the
// reader nothing.
func TestCodeHelpersSeeThroughWrapping(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		denied  bool
		limited bool
	}{
		{"denied, bare", &RPCError{Code: CodeCapabilityDenied, Message: "capability denied"}, true, false},
		{
			name:   "denied, wrapped twice as the real call stack wraps it",
			err:    fmt.Errorf("open command channel: %w", fmt.Errorf("chanbus: open exec channel: %w", &RPCError{Code: CodeCapabilityDenied})),
			denied: true,
		},
		{"rate limited, wrapped", fmt.Errorf("surface: %w", &RPCError{Code: CodeRateLimited}), false, true},
		{"another rpc error is neither", fmt.Errorf("x: %w", &RPCError{Code: -32603}), false, false},
		{"a plain error is neither", errors.New("connection reset"), false, false},
		{"nil is neither", nil, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsCapabilityDenied(tc.err); got != tc.denied {
				t.Errorf("IsCapabilityDenied(%v) = %v, want %v", tc.err, got, tc.denied)
			}
			if got := IsRateLimited(tc.err); got != tc.limited {
				t.Errorf("IsRateLimited(%v) = %v, want %v", tc.err, got, tc.limited)
			}
		})
	}
}
