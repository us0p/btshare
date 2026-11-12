package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSignVerify(t *testing.T) {
	secret := []byte("0123456789abcdef")
	body := []byte(`{"mac":"AA:BB:CC:DD:EE:FF"}`)
	now := time.Unix(1_700_000_000, 0)

	req := httptest.NewRequest(http.MethodPost, "/v1/acquire", nil)
	Sign(req, secret, body, now)

	if err := Verify(req, secret, body, now.Add(5*time.Second)); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	if err := Verify(req, []byte("wrong-secret-xxxxx"), body, now); err == nil {
		t.Fatal("wrong secret accepted")
	}
	if err := Verify(req, secret, []byte(`{"mac":"11:22:33:44:55:66"}`), now); err == nil {
		t.Fatal("tampered body accepted")
	}
	if err := Verify(req, secret, body, now.Add(2*time.Minute)); err == nil {
		t.Fatal("replayed (stale) request accepted")
	}
	other := httptest.NewRequest(http.MethodPost, "/v1/release", nil)
	other.Header = req.Header.Clone()
	if err := Verify(other, secret, body, now); err == nil {
		t.Fatal("signature reused on different path accepted")
	}
}
