package tools

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/ButterStack/butterstack-connector/internal/config"
)

// TeamCity talks REST to the studio's on-prem TeamCity server. The base URL and
// the Bearer token come from connector.yml and are never on the wire, which is
// the whole of survival condition 3 for this tool: revoking our connector token
// closes the socket and leaves the studio's TeamCity token untouched.
type TeamCity struct {
	base   *url.URL
	token  string
	client *http.Client
}

// serverInfoFields and buildFields are fixed projections. They are the client
// half of the egress spec: the connector asks TeamCity only for the fields the
// verb is documented to return, so a future TeamCity that adds fields does not
// silently widen what leaves the network.
const (
	serverInfoFields = "version,versionMajor,versionMinor,buildNumber,webUrl"
	buildFields      = "id,buildTypeId,number,status,state,statusText,branchName,webUrl," +
		"queuedDate,startDate,finishDate," +
		"revisions(revision(version,vcsBranchName,vcs-root-instance(id,vcs-root-id,vcsName)))"
)

// NewTeamCity builds an executor from the local config section.
func NewTeamCity(c config.TeamCity) (*TeamCity, error) {
	if !c.Enabled {
		return nil, ErrNotConfigured
	}
	u, err := url.Parse(strings.TrimRight(c.URL, "/"))
	if err != nil {
		return nil, fmt.Errorf("teamcity: url: %w", err)
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if c.CAFile != "" {
		pem, err := os.ReadFile(c.CAFile)
		if err != nil {
			return nil, fmt.Errorf("teamcity: ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("teamcity: ca_file contains no certificates")
		}
		tlsCfg.RootCAs = pool
	}
	// allow_insecure_tls applies to this LAN server only. There is deliberately
	// no equivalent for the broker connection.
	tlsCfg.InsecureSkipVerify = c.AllowInsecureTLS //nolint:gosec // documented, LAN-only, opt-in
	return &TeamCity{
		base:  u,
		token: c.Token,
		client: &http.Client{
			Timeout:   c.Timeout.D(),
			Transport: &http.Transport{TLSClientConfig: tlsCfg},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				// A redirect could send the Bearer token to another host.
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// ServerInfo is the projected result of teamcity.server.info.
type ServerInfo struct {
	Version      string `json:"version"`
	VersionMajor int    `json:"versionMajor"`
	VersionMinor int    `json:"versionMinor"`
	BuildNumber  string `json:"buildNumber"`
	WebURL       string `json:"webUrl"`
}

// Build is the projected result of teamcity.build.get.
type Build struct {
	ID          int64  `json:"id"`
	BuildTypeID string `json:"buildTypeId"`
	Number      string `json:"number"`
	Status      string `json:"status"`
	State       string `json:"state"`
	StatusText  string `json:"statusText"`
	BranchName  string `json:"branchName"`
	WebURL      string `json:"webUrl"`
	QueuedDate  string `json:"queuedDate"`
	StartDate   string `json:"startDate"`
	FinishDate  string `json:"finishDate"`
	Revisions   struct {
		Revision []struct {
			Version       string `json:"version"`
			VCSBranchName string `json:"vcsBranchName"`
		} `json:"revision"`
	} `json:"revisions"`
}

// Execute dispatches the TeamCity verbs.
func (t *TeamCity) Execute(ctx context.Context, verb string, args map[string]any, maxBytes int) (any, int, bool, error) {
	switch verb {
	case "teamcity.server.info":
		var out ServerInfo
		n, trunc, err := t.get(ctx, "/app/rest/server", serverInfoFields, maxBytes, &out)
		return out, n, trunc, err

	case "teamcity.build.get":
		id := argInt(args, "build_id", 0)
		var out Build
		path := fmt.Sprintf("/app/rest/builds/id:%d", id)
		n, trunc, err := t.get(ctx, path, buildFields, maxBytes, &out)
		return out, n, trunc, err
	}
	return nil, 0, false, fmt.Errorf("teamcity: no executor for %s", verb)
}

func (t *TeamCity) get(ctx context.Context, path, fields string, maxBytes int, into any) (int, bool, error) {
	u := *t.base
	u.Path = t.base.Path + path
	q := url.Values{}
	if fields != "" {
		q.Set("fields", fields)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, false, err
	}
	// The studio's TeamCity token rides this header to a host on the studio's
	// own LAN. It is never echoed into a result frame.
	req.Header.Set("Authorization", "Bearer "+t.token)
	req.Header.Set("Accept", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		return 0, false, fmt.Errorf("teamcity: %w", redactURL(err, t.token))
	}
	defer resp.Body.Close()

	if maxBytes <= 0 {
		maxBytes = 64 << 10
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxBytes)+1))
	if err != nil {
		return 0, false, fmt.Errorf("teamcity: read: %w", err)
	}
	truncated := len(body) > maxBytes
	if truncated {
		body = body[:maxBytes]
	}
	if resp.StatusCode != http.StatusOK {
		return len(body), truncated, fmt.Errorf("teamcity: HTTP %d", resp.StatusCode)
	}
	if err := json.Unmarshal(body, into); err != nil {
		if truncated {
			return len(body), true, fmt.Errorf("teamcity: response exceeded max_bytes")
		}
		return len(body), truncated, fmt.Errorf("teamcity: malformed response")
	}
	return len(body), truncated, nil
}

// Version reports the tool version string announced in hello, or empty when the
// server cannot be reached at startup. Failing to reach TeamCity is not a
// startup error: the connector still connects and answers sys verbs.
func (t *TeamCity) Version(ctx context.Context) string {
	var info ServerInfo
	if _, _, err := t.get(ctx, "/app/rest/server", serverInfoFields, 8192, &info); err != nil {
		return ""
	}
	return info.Version
}

// redactURL keeps a token out of a wrapped transport error, which would
// otherwise reach the audit log through the error string.
func redactURL(err error, secret string) error {
	if secret == "" {
		return err
	}
	s := strings.ReplaceAll(err.Error(), secret, "[redacted]")
	return fmt.Errorf("%s", s)
}
