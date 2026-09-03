// Package wsclient is a minimal RFC 6455 WebSocket client, client side only.
//
// It exists instead of a third-party dependency for two reasons. First, the
// connector's whole security story is "read the source": a studio's IT director
// can read this file in a sitting, which is not true of a general-purpose
// WebSocket library. Second, survival condition 1 asks for a small, auditable
// dependency surface, and a hand-written client that speaks the subset we
// actually use (client-initiated, masked text frames, ping/pong, close) is a
// materially smaller surface than one that also implements permessage-deflate,
// extensions, and a server half we never run.
//
// What this does NOT implement, on purpose: extensions, compression, subprotocol
// negotiation, and any server role. What it does implement strictly: the
// Sec-WebSocket-Accept check, client-side masking (required by RFC 6455 6.1),
// continuation frames up to a hard message cap, and control-frame size limits.
package wsclient

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// wsGUID is the RFC 6455 handshake constant.
const wsGUID = "258EAFA5-E914-47DA-95CA-5AB0DC85B11C"

// Opcodes.
const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
)

// Limits. A frame larger than these is a protocol error and closes the socket;
// the connector never needs a large inbound frame because commands are small
// and results flow the other way.
const (
	MaxMessageBytes = 1 << 20 // 1 MiB inbound message cap
	maxControlBytes = 125
)

// Errors callers distinguish.
var (
	ErrQueryString    = errors.New("wsclient: endpoint must not carry a query string")
	ErrHandshake      = errors.New("wsclient: handshake failed")
	ErrUnauthorized   = errors.New("wsclient: broker rejected the connector token")
	ErrMessageTooBig  = errors.New("wsclient: inbound message exceeds cap")
	ErrBinaryFrame    = errors.New("wsclient: binary frames are not part of this protocol")
	ErrClosedByServer = errors.New("wsclient: server closed the connection")
)

// Options configures a dial.
type Options struct {
	// Endpoint must be a wss:// URL with no query string. The connector token
	// is sent in the Authorization header and nowhere else.
	Endpoint string

	// Token is the bsc_ connector token.
	Token string

	// CAFile, when set, is the only root the TLS handshake will trust. It
	// exists for a private CA and for the drill harness; there is no option to
	// skip verification.
	CAFile string

	UserAgent      string
	DialTimeout    time.Duration
	HandshakeLimit time.Duration
}

// Conn is a live WebSocket connection.
type Conn struct {
	raw net.Conn
	br  *bufio.Reader

	writeMu sync.Mutex
	closed  bool
	closeMu sync.Mutex

	// lastActivity is the unix-nanos timestamp of the most recently received
	// frame of any kind, pong included. It is written from the single
	// goroutine that calls ReadMessage and read from any goroutine (the
	// session's idle-reconnect check), hence atomic rather than a plain field.
	lastActivity atomic.Int64
}

// LastActivity returns the time of the most recently received frame of any
// kind, including a pong answering our own ping. The protocol has no
// broker-to-connector traffic while idle apart from that pong, so this is
// what lets a read-deadline timeout distinguish "quiet but alive" from
// "actually dead" instead of treating every idle period as a dead socket.
// Zero until the first frame arrives.
func (c *Conn) LastActivity() time.Time {
	ns := c.lastActivity.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// Dial performs the TLS and WebSocket handshakes.
//
// The Authorization header is the only place the token appears. There is no
// code path in this package that writes the token into the request line, and
// a URL carrying a query string is refused before a socket is opened, so a
// misconfigured endpoint cannot leak a credential into a proxy access log.
func Dial(opts Options) (*Conn, error) {
	u, err := url.Parse(opts.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("wsclient: %w", err)
	}
	if u.Scheme != "wss" {
		return nil, fmt.Errorf("wsclient: endpoint scheme must be wss, got %q", u.Scheme)
	}
	if u.RawQuery != "" {
		return nil, ErrQueryString
	}
	if opts.DialTimeout == 0 {
		opts.DialTimeout = 10 * time.Second
	}
	if opts.HandshakeLimit == 0 {
		opts.HandshakeLimit = 15 * time.Second
	}

	host := u.Host
	if !strings.Contains(host, ":") {
		host += ":443"
	}

	tlsCfg := &tls.Config{
		ServerName: u.Hostname(),
		MinVersion: tls.VersionTLS12,
	}
	if opts.CAFile != "" {
		pem, err := os.ReadFile(opts.CAFile)
		if err != nil {
			return nil, fmt.Errorf("wsclient: endpoint_ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("wsclient: endpoint_ca_file contains no certificates")
		}
		tlsCfg.RootCAs = pool
	}

	dialer := &net.Dialer{Timeout: opts.DialTimeout}
	raw, err := tls.DialWithDialer(dialer, "tcp", host, tlsCfg)
	if err != nil {
		return nil, fmt.Errorf("wsclient: dial: %w", err)
	}

	_ = raw.SetDeadline(time.Now().Add(opts.HandshakeLimit))

	keyRaw := make([]byte, 16)
	if _, err := rand.Read(keyRaw); err != nil {
		raw.Close()
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(keyRaw)

	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	var req strings.Builder
	fmt.Fprintf(&req, "GET %s HTTP/1.1\r\n", path)
	fmt.Fprintf(&req, "Host: %s\r\n", u.Host)
	req.WriteString("Upgrade: websocket\r\n")
	req.WriteString("Connection: Upgrade\r\n")
	fmt.Fprintf(&req, "Sec-WebSocket-Key: %s\r\n", key)
	req.WriteString("Sec-WebSocket-Version: 13\r\n")
	fmt.Fprintf(&req, "Authorization: Bearer %s\r\n", opts.Token)
	if opts.UserAgent != "" {
		fmt.Fprintf(&req, "User-Agent: %s\r\n", opts.UserAgent)
	}
	req.WriteString("\r\n")

	if _, err := io.WriteString(raw, req.String()); err != nil {
		raw.Close()
		return nil, fmt.Errorf("wsclient: write handshake: %w", err)
	}

	br := bufio.NewReaderSize(raw, 8192)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		raw.Close()
		return nil, fmt.Errorf("%w: %v", ErrHandshake, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		raw.Close()
		return nil, fmt.Errorf("%w (HTTP %d)", ErrUnauthorized, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		raw.Close()
		return nil, fmt.Errorf("%w: HTTP %d", ErrHandshake, resp.StatusCode)
	}
	if !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") {
		raw.Close()
		return nil, fmt.Errorf("%w: missing Upgrade: websocket", ErrHandshake)
	}
	if got, want := resp.Header.Get("Sec-WebSocket-Accept"), AcceptKey(key); got != want {
		raw.Close()
		return nil, fmt.Errorf("%w: Sec-WebSocket-Accept mismatch", ErrHandshake)
	}

	_ = raw.SetDeadline(time.Time{})
	c := &Conn{raw: raw, br: br}
	c.lastActivity.Store(time.Now().UnixNano())
	return c, nil
}

// IsTimeout reports whether err is a read/write deadline expiring, as opposed
// to a genuine transport failure (connection reset, EOF, protocol error).
// The session's idle-reconnect check uses this to decide whether a
// ReadMessage deadline is cause to reconnect or just an idle broker.
func IsTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// AcceptKey computes the RFC 6455 Sec-WebSocket-Accept value. Exported so the
// drill harness can assert both halves agree.
func AcceptKey(clientKey string) string {
	h := sha1.New()
	io.WriteString(h, clientKey+wsGUID)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// WriteText sends one unfragmented masked text frame.
func (c *Conn) WriteText(p []byte) error { return c.writeFrame(opText, p) }

// WritePing sends a ping.
func (c *Conn) WritePing() error { return c.writeFrame(opPing, nil) }

// WriteClose sends a close frame with the given status code.
func (c *Conn) WriteClose(code uint16, reason string) error {
	buf := make([]byte, 2+len(reason))
	binary.BigEndian.PutUint16(buf, code)
	copy(buf[2:], reason)
	if len(buf) > maxControlBytes {
		buf = buf[:maxControlBytes]
	}
	return c.writeFrame(opClose, buf)
}

func (c *Conn) writeFrame(opcode byte, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	var hdr []byte
	b0 := byte(0x80) | opcode // FIN set; this client never fragments
	n := len(payload)
	switch {
	case n <= 125:
		hdr = []byte{b0, byte(0x80) | byte(n)}
	case n <= 0xFFFF:
		hdr = make([]byte, 4)
		hdr[0], hdr[1] = b0, 0x80|126
		binary.BigEndian.PutUint16(hdr[2:], uint16(n))
	default:
		hdr = make([]byte, 10)
		hdr[0], hdr[1] = b0, 0x80|127
		binary.BigEndian.PutUint64(hdr[2:], uint64(n))
	}

	// RFC 6455 6.1: a client MUST mask every frame it sends, with a fresh key.
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	masked := make([]byte, n)
	for i := 0; i < n; i++ {
		masked[i] = payload[i] ^ mask[i%4]
	}

	if err := c.raw.SetWriteDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return err
	}
	if _, err := c.raw.Write(hdr); err != nil {
		return err
	}
	if _, err := c.raw.Write(mask[:]); err != nil {
		return err
	}
	if n > 0 {
		if _, err := c.raw.Write(masked); err != nil {
			return err
		}
	}
	return nil
}

// ReadMessage returns the next complete text message, transparently answering
// pings and reassembling continuation frames. A read deadline is the connector's
// liveness check: if the broker stops heartbeating, the read fails and the
// session reconnects.
func (c *Conn) ReadMessage(deadline time.Time) ([]byte, error) {
	var msg []byte
	for {
		if err := c.raw.SetReadDeadline(deadline); err != nil {
			return nil, err
		}
		fin, opcode, payload, err := c.readFrame()
		if err != nil {
			return nil, err
		}
		c.lastActivity.Store(time.Now().UnixNano())
		switch opcode {
		case opPing:
			if err := c.writeFrame(opPong, payload); err != nil {
				return nil, err
			}
		case opPong:
			// liveness only; never surfaced as a message to the caller.
		case opClose:
			_ = c.writeFrame(opClose, payload)
			return nil, ErrClosedByServer
		case opBinary:
			return nil, ErrBinaryFrame
		case opText, opContinuation:
			if len(msg)+len(payload) > MaxMessageBytes {
				return nil, ErrMessageTooBig
			}
			msg = append(msg, payload...)
			if fin {
				return msg, nil
			}
		default:
			return nil, fmt.Errorf("wsclient: unknown opcode 0x%x", opcode)
		}
	}
}

func (c *Conn) readFrame() (fin bool, opcode byte, payload []byte, err error) {
	var h [2]byte
	if _, err = io.ReadFull(c.br, h[:]); err != nil {
		return
	}
	fin = h[0]&0x80 != 0
	if h[0]&0x70 != 0 {
		err = errors.New("wsclient: reserved bits set (no extensions are negotiated)")
		return
	}
	opcode = h[0] & 0x0F
	masked := h[1]&0x80 != 0
	length := uint64(h[1] & 0x7F)

	switch length {
	case 126:
		var e [2]byte
		if _, err = io.ReadFull(c.br, e[:]); err != nil {
			return
		}
		length = uint64(binary.BigEndian.Uint16(e[:]))
	case 127:
		var e [8]byte
		if _, err = io.ReadFull(c.br, e[:]); err != nil {
			return
		}
		length = binary.BigEndian.Uint64(e[:])
	}

	isControl := opcode&0x8 != 0
	if isControl {
		if !fin {
			err = errors.New("wsclient: fragmented control frame")
			return
		}
		if length > maxControlBytes {
			err = errors.New("wsclient: oversized control frame")
			return
		}
	}
	if length > MaxMessageBytes {
		err = ErrMessageTooBig
		return
	}

	var mask [4]byte
	if masked {
		// A server must not mask, but tolerate and unmask rather than desync.
		if _, err = io.ReadFull(c.br, mask[:]); err != nil {
			return
		}
	}
	payload = make([]byte, length)
	if _, err = io.ReadFull(c.br, payload); err != nil {
		return
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return
}

// Close shuts the socket down. Safe to call more than once.
func (c *Conn) Close() error {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	_ = c.WriteClose(1000, "going away")
	return c.raw.Close()
}

// LocalAddr reports the local side, used by the drills to correlate with ss/netstat.
func (c *Conn) LocalAddr() net.Addr { return c.raw.LocalAddr() }
