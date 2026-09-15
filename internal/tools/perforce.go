package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/ButterStack/butterstack-connector/internal/config"
)

// Perforce shells out to the p4 CLI.
//
// Every invocation is an argv array passed straight to execve: there is no
// shell in the path, so a depot path is data to p4 and can never be a command.
// (Shuri F5's smaller sibling: "if the connector shells out to the p4 CLI
// rather than using P4Ruby, every invocation must be an argv array with no
// shell interpretation.") The port, user, and ticket come from connector.yml
// and are passed as flags; the ticket is handed over through the environment's
// P4PASSWD rather than argv so it does not appear in the host's process list.
type Perforce struct {
	cfg config.Perforce
}

// NewPerforce builds an executor from the local config section.
func NewPerforce(c config.Perforce) (*Perforce, error) {
	if !c.Enabled {
		return nil, ErrNotConfigured
	}
	return &Perforce{cfg: c}, nil
}

// DescribedFile is one file in a changelist.
type DescribedFile struct {
	DepotFile string `json:"depot_file"`
	Action    string `json:"action"`
	Type      string `json:"type"`
	Rev       string `json:"rev"`
}

// Describe is the projected result of p4.describe. No diff, ever, in v0: the
// verb's include_diff argument is schema-pinned to false.
type Describe struct {
	Change      int64           `json:"change"`
	User        string          `json:"user"`
	Client      string          `json:"client"`
	Time        string          `json:"time"`
	Description string          `json:"description"`
	Status      string          `json:"status"`
	Files       []DescribedFile `json:"files"`
	FileCount   int             `json:"file_count"`

	// The changelist's real file count, before max_files or max_bytes were
	// applied. FileCount is what was RETURNED; this is what EXISTS. A consumer
	// comparing the two can see exactly how much a truncated record is missing
	// instead of only knowing that it is short (#1904).
	DepotFileCount int `json:"depot_file_count"`
}

// ChangeSummary is one entry of p4.changes.
type ChangeSummary struct {
	Change      int64  `json:"change"`
	User        string `json:"user"`
	Time        string `json:"time"`
	Description string `json:"description"`
}

// Execute dispatches the Perforce verbs.
func (p *Perforce) Execute(ctx context.Context, verb string, args map[string]any, maxBytes int) (any, int, bool, error) {
	switch verb {
	case "p4.describe":
		change := argInt(args, "change", 0)
		maxFiles := int(argInt(args, "max_files", 200))
		return p.describe(ctx, change, maxFiles, maxBytes)

	case "p4.changes":
		path := argString(args, "path")
		max := int(argInt(args, "max", 25))
		return p.changes(ctx, path, max, maxBytes)
	}
	return nil, 0, false, fmt.Errorf("perforce: no executor for %s", verb)
}

func (p *Perforce) describe(ctx context.Context, change int64, maxFiles, maxBytes int) (any, int, bool, error) {
	// -s omits the diffs entirely; this is the content boundary enforced at the
	// tool invocation, not only in the schema.
	recs, n, err := p.run(ctx, "describe", "-s", strconv.FormatInt(change, 10))
	if err != nil {
		return nil, n, false, err
	}
	if len(recs) == 0 {
		return nil, n, false, fmt.Errorf("perforce: changelist %d not found", change)
	}
	out, truncated := describeFromRecord(recs[0], change, maxFiles, maxBytes)
	return out, n, truncated, nil
}

// describeFromRecord shapes one p4 -Mj describe record into a Describe,
// applying both size bounds. Split out from describe() so the bounds - which
// are the whole subject of #1904 - can be exercised without a live p4 server.
//
// TWO bounds apply, and both TRUNCATE rather than fail (PROTOCOL.md,
// "Ordering, replay, and size": "the connector truncates and sets
// truncated: true rather than streaming unbounded"). max_bytes used to
// short-circuit in run() with an error instead, which is why changelist 118 on
// project 108 - 503 files, an ordinary asset reorganisation for a game studio -
// never synced at all rather than syncing partially.
func describeFromRecord(r map[string]any, change int64, maxFiles, maxBytes int) (Describe, bool) {
	out := Describe{
		Change:      change,
		User:        str(r, "user"),
		Client:      str(r, "client"),
		Time:        str(r, "time"),
		Description: str(r, "desc"),
		Status:      str(r, "status"),
	}

	// p4 -Mj returns indexed keys: depotFile0, action0, type0, rev0, ...
	//
	// depotFileCount is the changelist's REAL file count, counted before either
	// bound is applied. Without it a truncated record cannot be told from a
	// complete one by size alone, and the consumer has no way to know how much
	// it is missing - FileCount below is only what was returned.
	truncated := false
	budget := maxBytes
	if budget <= 0 {
		budget = DefaultDescribeMaxBytes
	}

	depotFileCount := 0
	for i := 0; ; i++ {
		df := str(r, "depotFile"+strconv.Itoa(i))
		if df == "" {
			break
		}
		depotFileCount++

		if truncated {
			continue // keep counting; stop collecting
		}
		if len(out.Files) >= maxFiles {
			truncated = true
			continue
		}

		file := DescribedFile{
			DepotFile: df,
			Action:    str(r, "action"+strconv.Itoa(i)),
			Type:      str(r, "type"+strconv.Itoa(i)),
			Rev:       str(r, "rev"+strconv.Itoa(i)),
		}
		// Charged against the budget before appending, so the bound holds
		// rather than being discovered one record after it was crossed.
		cost := file.approxJSONBytes()
		if cost > budget {
			truncated = true
			continue
		}
		budget -= cost

		out.Files = append(out.Files, file)
	}
	out.FileCount = len(out.Files)
	out.DepotFileCount = depotFileCount
	return out, truncated
}

// approxJSONBytes estimates what this record costs in the encoded response.
// An estimate is the right tool: the exact cost depends on the encoder's
// escaping, and re-marshalling every record to find out would make the bound
// more expensive than the work it bounds. It counts the field values plus a
// fixed allowance for the keys, braces, quotes and commas, and it rounds UP,
// so the real response is never larger than the budget implies.
func (f DescribedFile) approxJSONBytes() int {
	const envelope = 64 // {"depot_file":"","action":"","type":"","rev":""},
	return envelope + len(f.DepotFile) + len(f.Action) + len(f.Type) + len(f.Rev)
}

func (p *Perforce) changes(ctx context.Context, path string, max, maxBytes int) (any, int, bool, error) {
	recs, n, err := p.run(ctx, "changes", "-m", strconv.Itoa(max), path)
	if err != nil {
		return nil, n, false, err
	}
	out := make([]ChangeSummary, 0, len(recs))
	for _, r := range recs {
		c, _ := strconv.ParseInt(str(r, "change"), 10, 64)
		out = append(out, ChangeSummary{
			Change:      c,
			User:        str(r, "user"),
			Time:        str(r, "time"),
			Description: str(r, "desc"),
		})
	}
	return out, n, false, nil
}

// Size bounds.
//
// These used to be metadata-sized (64 KiB, roughly 500 file records) and they
// decided which changelists were allowed to be CORRECT rather than protecting
// anything: ButterStack already stores the full file list for every changelist
// under the limit, so the cap never stopped the data existing in the product.
// An asset reorganisation of a few hundred files is ordinary for a game studio
// and is exactly what this daemon exists to observe.
//
// So the defaults are now sized for the work, and the true ceiling is a
// separate, much larger memory bound.
const (
	// DefaultDescribeMaxBytes is the response budget for p4.describe when the
	// caller names none. ~4 MiB is roughly 30,000 file records: past any real
	// changelist, well short of a memory problem.
	DefaultDescribeMaxBytes = 4 << 20

	// MaxToolOutputBytes is how much raw p4 output the daemon will hold for a
	// single command, so a pathological changelist cannot exhaust it. This is
	// the sanity bound; it is not a response-shaping knob and no caller can
	// raise it.
	MaxToolOutputBytes = 64 << 20
)

// cappedBuffer is a bytes.Buffer that stops growing past limit and records
// that it did. Writing into a plain buffer and checking its length afterwards
// would already have allocated whatever p4 produced, which is the allocation
// the ceiling exists to prevent.
type cappedBuffer struct {
	buf        bytes.Buffer
	limit      int
	overflowed bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if c.overflowed {
		return len(p), nil // absorb the rest; the command is already doomed
	}
	if remaining := c.limit - c.buf.Len(); len(p) > remaining {
		c.overflowed = true
		if remaining > 0 {
			c.buf.Write(p[:remaining])
		}
		return len(p), nil
	}
	return c.buf.Write(p)
}

func (c *cappedBuffer) Bytes() []byte { return c.buf.Bytes() }
func (c *cappedBuffer) Len() int      { return c.buf.Len() }

// run invokes p4 with -Mj (one JSON object per record) and returns the parsed
// records. args is appended to a fixed prefix; nothing in args is interpreted.
func (p *Perforce) run(ctx context.Context, args ...string) ([]map[string]any, int, error) {
	argv := append([]string{
		"-p", p.cfg.Port,
		"-u", p.cfg.User,
		"-Mj", "-ztag",
	}, args...)

	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout.D())
	defer cancel()

	cmd := exec.CommandContext(ctx, p.cfg.Binary, argv...) // argv array; no shell
	cmd.Env = p.env()
	// The per-verb max_bytes budget is NOT enforced here. It is a response-
	// shaping bound and belongs where records are assembled, so a large
	// changelist truncates with truncated: true rather than failing outright.
	// What is enforced here is a memory ceiling: a bound on what the daemon is
	// willing to hold, not on what the caller asked for. It is measured in
	// megabytes and a changelist that trips it is genuinely pathological.
	stdout := &cappedBuffer{limit: MaxToolOutputBytes}
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = stdout, &stderr

	if err := cmd.Run(); err != nil {
		return nil, stdout.Len(), fmt.Errorf("perforce: %s", failureDetail(stdout.Bytes(), stderr.String(), err))
	}
	if stdout.overflowed {
		return nil, stdout.Len(), fmt.Errorf("perforce: output exceeded the %d-byte tool ceiling", MaxToolOutputBytes)
	}

	// Decode from a reader over the bytes rather than from the buffer itself.
	// Decoding CONSUMES a bytes.Buffer, so the previous `json.NewDecoder(&stdout)`
	// left it empty and the `stdout.Len()` reported below was therefore always
	// 0 - every successful command reported `bytes: 0` to the broker and into
	// the audit log. Found while removing the max_bytes hard-fail above, which
	// was the only caller that read the length before the decode drained it.
	raw := stdout.Bytes()
	n := len(raw)

	var recs []map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	for {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			break
		}
		recs = append(recs, m)
	}
	return recs, n, nil
}

// env builds a minimal environment. The ticket goes in P4PASSWD rather than on
// the command line so it never shows up in `ps` on the studio's host.
func (p *Perforce) env() []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"P4PORT=" + p.cfg.Port,
		"P4USER=" + p.cfg.User,
	}
	if p.cfg.Ticket != "" {
		env = append(env, "P4PASSWD="+p.cfg.Ticket)
	}
	return env
}

func str(m map[string]any, k string) string {
	if v, ok := m[k]; ok {
		if s, ok := v.(string); ok {
			return s
		}
		return fmt.Sprint(v)
	}
	return ""
}

// failureDetail extracts the most specific description available for a failed
// p4 invocation.
//
// Under -Mj the p4 client writes its error records to STDOUT as JSON, not to
// stderr. Reading stderr alone therefore left every tool failure reported as
// the bare wait-status text - "perforce: exit status 1" - with no indication of
// what actually went wrong. That cost four diagnoses before it was fixed: a
// missing trust file and an expired ticket on 2026-09-08, and the two failures
// in the 2026-09-14 Pilot Light replay, including changelist 118, whose cause
// is still unknown for exactly this reason.
//
// Precedence is most-specific-first: an error record from p4 itself, then
// stderr (which still carries connection-level failures the client never got
// far enough to report as a record), then the process wait status, which is
// always available and always useless on its own.
func failureDetail(stdout []byte, stderr string, runErr error) string {
	if msg := firstErrorRecord(stdout); msg != "" {
		return msg
	}
	if msg := strings.TrimSpace(stderr); msg != "" {
		return firstLine(msg)
	}
	return firstLine(runErr.Error())
}

// firstErrorRecord returns the `data` field of the first -Mj record reporting a
// failure, or "" if there is none.
//
// A -Mj stream is one JSON object per record, and a failed command can emit
// several - p4 reports each error separately, and an error stream may also
// carry successful records before it. Records are identified by their
// "generic" and "severity" fields; anything at severity 3 (failed) or above is
// an error, and a record carrying `data` with no severity at all is treated as
// one too, since p4 is not perfectly consistent about emitting severity and the
// text is what the caller needs either way.
//
// Malformed or truncated JSON is not an error here: the decoder stops at the
// first record it cannot read and whatever was found before that still stands.
// This runs on a path that is ALREADY failing, so it must never panic or
// introduce a second failure on top of the first.
func firstErrorRecord(stdout []byte) string {
	dec := json.NewDecoder(bytes.NewReader(stdout))
	for {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			return ""
		}

		data := strings.TrimSpace(str(m, "data"))
		if data == "" {
			continue
		}

		if severity, ok := numField(m, "severity"); ok {
			if severity >= p4SeverityFailed {
				return firstLine(data)
			}
			continue
		}

		return firstLine(data)
	}
}

// p4's own severity scale: 0 empty, 1 info, 2 warning, 3 failed, 4 fatal.
const p4SeverityFailed = 3

// numField reads a field p4 may render as either a JSON number or a quoted
// string ("3" and 3 both occur across versions and record types).
func numField(m map[string]any, k string) (float64, bool) {
	switch v := m[k].(type) {
	case float64:
		return v, true
	case string:
		n, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
