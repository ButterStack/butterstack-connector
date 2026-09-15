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
	recs, n, err := p.run(ctx, maxBytes, "describe", "-s", strconv.FormatInt(change, 10))
	if err != nil {
		return nil, n, false, err
	}
	if len(recs) == 0 {
		return nil, n, false, fmt.Errorf("perforce: changelist %d not found", change)
	}
	r := recs[0]
	out := Describe{
		Change:      change,
		User:        str(r, "user"),
		Client:      str(r, "client"),
		Time:        str(r, "time"),
		Description: str(r, "desc"),
		Status:      str(r, "status"),
	}
	// p4 -Mj returns indexed keys: depotFile0, action0, type0, rev0, ...
	truncated := false
	for i := 0; ; i++ {
		df := str(r, "depotFile"+strconv.Itoa(i))
		if df == "" {
			break
		}
		if len(out.Files) >= maxFiles {
			truncated = true
			break
		}
		out.Files = append(out.Files, DescribedFile{
			DepotFile: df,
			Action:    str(r, "action"+strconv.Itoa(i)),
			Type:      str(r, "type"+strconv.Itoa(i)),
			Rev:       str(r, "rev"+strconv.Itoa(i)),
		})
	}
	out.FileCount = len(out.Files)
	return out, n, truncated, nil
}

func (p *Perforce) changes(ctx context.Context, path string, max, maxBytes int) (any, int, bool, error) {
	recs, n, err := p.run(ctx, maxBytes, "changes", "-m", strconv.Itoa(max), path)
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

// run invokes p4 with -Mj (one JSON object per record) and returns the parsed
// records. args is appended to a fixed prefix; nothing in args is interpreted.
func (p *Perforce) run(ctx context.Context, maxBytes int, args ...string) ([]map[string]any, int, error) {
	argv := append([]string{
		"-p", p.cfg.Port,
		"-u", p.cfg.User,
		"-Mj", "-ztag",
	}, args...)

	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout.D())
	defer cancel()

	cmd := exec.CommandContext(ctx, p.cfg.Binary, argv...) // argv array; no shell
	cmd.Env = p.env()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	if err := cmd.Run(); err != nil {
		return nil, stdout.Len(), fmt.Errorf("perforce: %s", failureDetail(stdout.Bytes(), stderr.String(), err))
	}
	if maxBytes > 0 && stdout.Len() > maxBytes {
		return nil, stdout.Len(), fmt.Errorf("perforce: response exceeded max_bytes")
	}

	var recs []map[string]any
	dec := json.NewDecoder(&stdout)
	for {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			break
		}
		recs = append(recs, m)
	}
	return recs, stdout.Len(), nil
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
