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
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, stdout.Len(), fmt.Errorf("perforce: %s", firstLine(msg))
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

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
