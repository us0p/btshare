// Package auth signs and verifies agent requests with an HMAC over a shared
// secret, a timestamp and the request body, so only hosts holding the secret
// can move devices around and captured requests can't be replayed later.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

const (
	HeaderTS  = "X-Btshare-Timestamp"
	HeaderSig = "X-Btshare-Signature"
	MaxSkew   = 30 * time.Second
)

func mac(secret []byte, method, path, ts string, body []byte) string {
	m := hmac.New(sha256.New, secret)
	fmt.Fprintf(m, "%s\n%s\n%s\n", method, path, ts)
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// Sign adds auth headers to req. body must be the exact request body.
func Sign(req *http.Request, secret []byte, body []byte, now time.Time) {
	ts := strconv.FormatInt(now.Unix(), 10)
	req.Header.Set(HeaderTS, ts)
	req.Header.Set(HeaderSig, mac(secret, req.Method, req.URL.Path, ts, body))
}

// Verify checks the auth headers of an incoming request.
func Verify(req *http.Request, secret []byte, body []byte, now time.Time) error {
	ts := req.Header.Get(HeaderTS)
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return errors.New("missing or bad timestamp")
	}
	if d := now.Sub(time.Unix(sec, 0)); d > MaxSkew || d < -MaxSkew {
		return fmt.Errorf("timestamp skew %s too large (check clocks/NTP)", d.Round(time.Second))
	}
	want := mac(secret, req.Method, req.URL.Path, ts, body)
	if !hmac.Equal([]byte(want), []byte(req.Header.Get(HeaderSig))) {
		return errors.New("bad signature")
	}
	return nil
}
