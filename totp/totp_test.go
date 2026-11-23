package totp

import (
	"testing"
	"time"
)

// RFC 6238 appendix B's SHA-1 vectors (secret "12345678901234567890"), cut to six digits as providers use them.
func TestCodeMatchesTheRFCVectors(t *testing.T) {
	const rfcKey = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	for _, tc := range []struct {
		at   int64
		want string
	}{{59, "287082"}, {1111111109, "081804"}, {1111111111, "050471"}, {1234567890, "005924"}, {2000000000, "279037"}, {20000000000, "353130"}} {
		if got, err := Code(rfcKey, Step(time.Unix(tc.at, 0))); got != tc.want || err != nil {
			t.Errorf("Code(rfcKey, Step(%d)) = %q, %v; want %q", tc.at, got, err, tc.want)
		}
	}
	if _, err := Code("not base32!", 1); err == nil {
		t.Error("Code(an invalid secret) = no error, want one")
	}
}
