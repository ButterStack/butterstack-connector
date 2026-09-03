package wsclient

import (
	"bufio"
	"errors"
	"net"
	"testing"
	"time"
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

// TestPongUpdatesLastActivity is the regression guard for the idle-reconnect
// fix: a pong is "silent" (ReadMessage never returns it as a message, per the
// opPong case in the read loop), but it must still count as activity, or the
// session's idle check would treat every quiet-but-alive period as dead. Built
// directly against a Conn over a net.Pipe rather than through Dial, since Dial
// requires a full HTTP upgrade handshake this test has no need for.
func TestPongUpdatesLastActivity(t *testing.T) {
	serverSide, clientSide := net.Pipe()
	defer serverSide.Close()
	defer clientSide.Close()

	c := &Conn{raw: clientSide, br: bufio.NewReader(clientSide)}
	if !c.LastActivity().IsZero() {
		t.Fatalf("LastActivity should be zero before any frame arrives")
	}

	// A minimal, unmasked pong frame (servers must not mask, per RFC 6455):
	// FIN+opcode byte 0x8A, then a zero-length payload byte 0x00.
	go func() {
		_, _ = serverSide.Write([]byte{0x8A, 0x00})
	}()

	before := time.Now()
	// The pong never completes a message, so ReadMessage blocks until its
	// deadline; that timeout is expected here; what matters is LastActivity
	// having moved in the meantime.
	_, err := c.ReadMessage(time.Now().Add(200 * time.Millisecond))
	if err == nil {
		t.Fatalf("expected a deadline timeout (only a pong was sent, no message)")
	}
	if !IsTimeout(err) {
		t.Fatalf("expected a timeout error, got %v", err)
	}

	got := c.LastActivity()
	if got.IsZero() {
		t.Fatalf("LastActivity was not updated by the pong")
	}
	if got.Before(before) {
		t.Fatalf("LastActivity = %v, should be at or after %v", got, before)
	}
}
