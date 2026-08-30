package wsclient

import (
	"errors"
	"testing"
)

// TestAcceptKey pins base64(sha1(key + RFC 6455 GUID)) for the RFC's sample
// key. The value is cross-checked against two independent implementations: a
// one-line Python hashlib computation, and the Ruby handshake in
// connector/test/support/ws.rb, which the drill harness runs against this
// client for real. A regression here would show up as a failed handshake in
// every drill, not just as a failed unit test.
func TestAcceptKey(t *testing.T) {
	if got, want := AcceptKey("dGhlIHNhbXBsZSBub25jZQ=="), "84qioe71YN9dzYnTCQMk2L+0/kA="; got != want {
		t.Fatalf("AcceptKey = %q, want %q", got, want)
	}
}

// TestDialRefusesAQueryString is the client half of drill (g). The token is a
// header value and nothing else, so an endpoint that carries a query string is
// refused before a socket is opened.
func TestDialRefusesAQueryString(t *testing.T) {
	_, err := Dial(Options{Endpoint: "wss://example.invalid/connect?token=bsc_a_b", Token: "bsc_a_b"})
	if !errors.Is(err, ErrQueryString) {
		t.Fatalf("want ErrQueryString, got %v", err)
	}
}

func TestDialRefusesPlaintextSchemes(t *testing.T) {
	for _, ep := range []string{"ws://example.invalid/connect", "http://example.invalid/connect"} {
		if _, err := Dial(Options{Endpoint: ep}); err == nil {
			t.Errorf("%s was accepted", ep)
		}
	}
}
