// Package totp gives the time-based one-time codes of RFC 6238 that providers ask a test user for: HMAC-SHA1, six
// digits, 30-second steps, a base32 secret as authenticator apps take it.
package totp

import (
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // RFC 6238's TOTP is defined over HMAC-SHA1, which providers use; it is no hash of a secret to store
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"strings"
	"time"
)

// Period is the length of one step.
const Period = 30 * time.Second

// Step is the step that instant t falls in.
func Step(t time.Time) int64 {
	return t.Unix() / int64(Period/time.Second)
}

// Code is the six-digit code of a base32 secret (upper or lower case, padding optional) for one step.
func Code(secret string, step int64) (string, error) {
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(strings.TrimRight(secret, "=")))
	if err != nil {
		return "", fmt.Errorf("the one-time-code secret is not base32: %w", err)
	}
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(step)) //nolint:gosec // a step since 1970 is never negative
	mac := hmac.New(sha1.New, key)
	mac.Write(counter[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	return fmt.Sprintf("%06d", binary.BigEndian.Uint32(sum[offset:offset+4])&0x7fffffff%1_000_000), nil
}
