// Package session is the connector's run loop: dial, hello, heartbeat,
// dispatch, reconnect.
//
// The loop never gives up and never escalates. A broker that is down, a token
// that was revoked, and a network that dropped all produce the same studio-side
// behaviour: a log line and a backoff. That is survival condition 6 seen from
// the inside -- stopping the connector, or losing it, is a state change, not an
// outage.
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/ButterStack/butterstack-connector/internal/audit"
	"github.com/ButterStack/butterstack-connector/internal/config"
	"github.com/ButterStack/butterstack-connector/internal/protocol"
	"github.com/ButterStack/butterstack-connector/internal/tools"
	"github.com/ButterStack/butterstack-connector/internal/vocab"
	"github.com/ButterStack/butterstack-connector/internal/wsclient"
)

// Backoff bounds. Capped and jittered so a fleet of connectors reconnecting
// after a deploy does not arrive in lockstep.
const (
	backoffMin = 500 * time.Millisecond
	backoffMax = 60 * time.Second

	defaultHeartbeat = 25 * time.Second
	minHeartbeat     = 5 * time.Second
	maxHeartbeat     = 120 * time.Second

	// readSlack is how long past the expected heartbeat interval the connector
	// waits before concluding the socket is dead.
	readSlack = 2

	// dedupeWindow is the sliding window for duplicate command ids.
	dedupeWindow  = 10 * time.Minute
	dedupeMaxKeys = 4096

	maxDeadline     = 60 * time.Second
	defaultDeadline = 15 * time.Second
)

// Runner owns one connector process's connection lifecycle.
type Runner struct {
	cfg     *config.Config
	log     *audit.Logger
	version string

	execs map[string]tools.Executor

	mu        sync.Mutex
	seen      map[string]time.Time
	sessionID string
}

// New builds a Runner. Executors are constructed once, from config, so no
// per-command code path can choose a different host or credential.
func New(cfg *config.Config, log *audit.Logger, version string) (*Runner, error) {
	if err := vocab.Selfcheck(); err != nil {
		return nil, err
	}
	r := &Runner{cfg: cfg, log: log, version: version, execs: map[string]tools.Executor{}, seen: map[string]time.Time{}}

	if cfg.TeamCity.Enabled {
		tc, err := tools.NewTeamCity(cfg.TeamCity)
		if err != nil {
			return nil, err
		}
		r.execs["teamcity"] = tc
	}
	if cfg.Perforce.Enabled {
		p4, err := tools.NewPerforce(cfg.Perforce)
		if err != nil {
			return nil, err
		}
		r.execs["perforce"] = p4
	}
	return r, nil
}

// Run blocks until ctx is cancelled, reconnecting forever.
func (r *Runner) Run(ctx context.Context) error {
	attempt := 0
	for {
		if ctx.Err() != nil {
			return nil
		}
		connected, err := r.runOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if connected {
			// A session that actually came up resets the backoff, so a long
			// uptime is not punished by an old failure streak.
			attempt = 0
		}
		attempt++
		d := backoff(attempt)
		reason := "connection closed"
		if err != nil {
			reason = err.Error()
		}
		r.log.Write(audit.Entry{
			Event:   "reconnect_scheduled",
			Reason:  classify(err),
			Message: fmt.Sprintf("%s; retrying in %s (attempt %d)", reason, d.Round(time.Millisecond), attempt),
		})
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(d):
		}
	}
}

// classify turns an error into a stable token the drills can assert on.
func classify(err error) string {
	switch {
	case err == nil:
		return "closed"
	case errors.Is(err, wsclient.ErrUnauthorized):
		return "unauthorized"
	case errors.Is(err, wsclient.ErrClosedByServer):
		return "closed_by_broker"
	case errors.Is(err, wsclient.ErrQueryString):
		return "endpoint_query_string"
	default:
		return "transport_error"
	}
}

func backoff(attempt int) time.Duration {
	d := backoffMin << min(attempt-1, 12)
	if d > backoffMax {
		d = backoffMax
	}
	// Full jitter, so a fleet does not stampede.
	return time.Duration(rand.Int63n(int64(d)) + int64(backoffMin))
}

func (r *Runner) runOnce(ctx context.Context) (connected bool, err error) {
	conn, err := wsclient.Dial(wsclient.Options{
		Endpoint:  r.cfg.Endpoint,
		Token:     r.cfg.Token,
		CAFile:    r.cfg.EndpointCAFile,
		UserAgent: "butterstack-connector/" + r.version,
	})
	if err != nil {
		return false, err
	}
	defer conn.Close()

	hello := protocol.NewHello(
		r.cfg.ConnectorID,
		r.cfg.IntegrationID(),
		r.version,
		vocab.CompiledVerbs(),
		r.toolVersions(ctx),
	)
	if err := writeJSON(conn, hello); err != nil {
		return false, err
	}

	raw, err := conn.ReadMessage(time.Now().Add(30 * time.Second))
	if err != nil {
		return false, err
	}
	kind, err := protocol.DecodeEnvelope(raw)
	if err != nil {
		return false, err
	}
	heartbeat := defaultHeartbeat
	switch kind {
	case protocol.TypeWelcome:
		var w protocol.Welcome
		if err := json.Unmarshal(raw, &w); err != nil {
			return false, err
		}
		if w.HeartbeatInterval > 0 {
			hb := time.Duration(w.HeartbeatInterval) * time.Second
			if hb >= minHeartbeat && hb <= maxHeartbeat {
				heartbeat = hb
			}
		}
		r.mu.Lock()
		r.sessionID = w.SessionID
		r.mu.Unlock()
		r.log.Write(audit.Entry{Event: "connected", SessionID: w.SessionID,
			Message: fmt.Sprintf("broker accepted; heartbeat %s", heartbeat)})
	case protocol.TypeReject:
		var rj protocol.Reject
		_ = json.Unmarshal(raw, &rj)
		r.log.Write(audit.Entry{Event: "rejected", Reason: rj.Reason})
		return false, fmt.Errorf("broker rejected the session: %s", rj.Reason)
	default:
		return false, fmt.Errorf("expected welcome or reject, got %q", kind)
	}

	return true, r.pump(ctx, conn, heartbeat)
}

// pump runs the heartbeat ticker and the read loop until either fails.
func (r *Runner) pump(ctx context.Context, conn *wsclient.Conn, heartbeat time.Duration) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	errCh := make(chan error, 1)
	sem := make(chan struct{}, r.cfg.MaxConcurrent)

	// fail reports the first error without ever blocking: a goroutine that
	// cannot report must still be able to return, or the shutdown path hangs.
	fail := func(err error) {
		select {
		case errCh <- err:
		default:
		}
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(heartbeat)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				hb := protocol.Heartbeat{Type: protocol.TypeHeartbeat,
					TS: time.Now().UTC().Format(time.RFC3339), QueueDepth: len(sem)}
				if err := writeJSON(conn, hb); err != nil {
					fail(err)
					return
				}
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			// If the broker goes quiet for longer than a couple of heartbeat
			// intervals the socket is dead even if TCP has not noticed.
			raw, err := conn.ReadMessage(time.Now().Add(heartbeat * readSlack))
			if err != nil {
				fail(err)
				return
			}
			kind, err := protocol.DecodeEnvelope(raw)
			if err != nil {
				r.log.Write(audit.Entry{Event: "frame_dropped", Reason: "malformed"})
				continue
			}
			if kind != protocol.TypeCommand {
				// An older connector must tolerate a newer broker's frames.
				r.log.Write(audit.Entry{Event: "frame_ignored", Message: kind})
				continue
			}
			var cmd protocol.Command
			if err := json.Unmarshal(raw, &cmd); err != nil {
				r.log.Write(audit.Entry{Event: "frame_dropped", Reason: "malformed_command"})
				continue
			}
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				res := r.handle(ctx, cmd)
				if err := writeJSON(conn, res); err != nil {
					fail(err)
				}
			}()
		}
	}()

	select {
	case err := <-errCh:
		cancel()
		conn.Close()
		wg.Wait()
		return err
	case <-ctx.Done():
		conn.Close()
		wg.Wait()
		return nil
	}
}

// handle is the command path: dedupe, resolve against the vocabulary, execute,
// and write exactly one audit line whatever the outcome.
func (r *Runner) handle(ctx context.Context, cmd protocol.Command) protocol.Result {
	start := time.Now()
	digest := audit.ArgsDigest(cmd.Args)

	r.mu.Lock()
	sessionID := r.sessionID
	r.mu.Unlock()

	finish := func(res protocol.Result, detail string) protocol.Result {
		r.log.Write(audit.Entry{
			Event:      "command",
			SessionID:  sessionID,
			CommandID:  cmd.ID,
			Verb:       cmd.Verb,
			ArgsSHA256: digest,
			Status:     res.Status,
			Reason:     res.Reason,
			Detail:     detail,
			BytesOut:   res.Bytes,
			Truncated:  res.Truncated,
			DurationMS: time.Since(start).Milliseconds(),
		})
		return res
	}

	if cmd.ID == "" {
		return finish(protocol.Deny("", vocab.ReasonMalformedArgs), "command has no id")
	}
	if !r.claim(cmd.ID) {
		return finish(protocol.Deny(cmd.ID, vocab.ReasonDuplicateCommandID), "")
	}

	verb, args, derr := vocab.Resolve(cmd.Verb, cmd.Args, vocab.Scopes{
		DepotScope:        r.cfg.Scopes.DepotScope,
		AllowedBuildTypes: r.cfg.Scopes.AllowedBuildTypes,
		RepoAllowlist:     r.cfg.Scopes.RepoAllowlist,
	}, r.cfg.Tools())
	if derr != nil {
		return finish(protocol.Deny(cmd.ID, derr.Reason), derr.Detail)
	}

	deadline := time.Duration(cmd.DeadlineMs) * time.Millisecond
	if deadline <= 0 {
		deadline = defaultDeadline
	}
	if deadline > maxDeadline {
		deadline = maxDeadline
	}
	cctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	maxBytes := cmd.MaxBytes
	if maxBytes <= 0 || maxBytes > verb.DefaultMaxBytes {
		maxBytes = verb.DefaultMaxBytes
	}

	body, n, truncated, err := r.execute(cctx, verb, args, maxBytes)
	switch {
	case errors.Is(cctx.Err(), context.DeadlineExceeded):
		res := protocol.Result{Type: protocol.TypeResult, ID: cmd.ID,
			Status: protocol.StatusTimeout, Reason: vocab.ReasonDeadlineExceeded}
		return finish(res, "")
	case err != nil:
		return finish(protocol.Errorf(cmd.ID, "%s", err.Error()), "")
	}
	return finish(protocol.OK(cmd.ID, body, n, truncated), "")
}

func (r *Runner) execute(ctx context.Context, verb *vocab.Verb, args map[string]any, maxBytes int) (any, int, bool, error) {
	switch verb.Name {
	case "sys.ping":
		return map[string]any{"pong": true, "ts": time.Now().UTC().Format(time.RFC3339Nano)}, 0, false, nil
	case "sys.version":
		return map[string]any{"version": r.version, "protocol": protocol.Version}, 0, false, nil
	case "sys.capabilities":
		return map[string]any{
			"verbs": vocab.CompiledVerbs(),
			"tools": r.cfg.Tools(),
		}, 0, false, nil
	}
	ex, ok := r.execs[verb.Tool]
	if !ok {
		return nil, 0, false, tools.ErrNotConfigured
	}
	return ex.Execute(ctx, verb.Name, args, maxBytes)
}

// claim implements the sliding-window duplicate check. A replayed command id is
// denied rather than re-executed, which matters most for the mutating verbs
// this version does not yet compile in.
func (r *Runner) claim(id string) bool {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.seen) > dedupeMaxKeys {
		for k, t := range r.seen {
			if now.Sub(t) > dedupeWindow {
				delete(r.seen, k)
			}
		}
	}
	if t, ok := r.seen[id]; ok && now.Sub(t) <= dedupeWindow {
		return false
	}
	r.seen[id] = now
	return true
}

func (r *Runner) toolVersions(ctx context.Context) map[string]string {
	out := map[string]string{}
	if tc, ok := r.execs["teamcity"].(interface{ Version(context.Context) string }); ok {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if v := tc.Version(cctx); v != "" {
			out["teamcity"] = v
		}
	}
	return out
}

func writeJSON(conn *wsclient.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return conn.WriteText(b)
}
