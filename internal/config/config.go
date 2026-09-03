// Package config loads connector.yml.
//
// Survival condition 3 is local credential custody, and the way this package
// keeps that promise is narrow and testable: every credential the connector
// uses is read from the YAML file the studio wrote, or from a *_file path that
// YAML names. There is deliberately no environment-variable fallback, no
// command-line flag that takes a secret, and no remote configuration: the
// broker cannot tell the connector where to find a credential, because if it
// could, "your credentials never leave your network" would depend on our good
// behaviour rather than on the studio's file permissions.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// tokenPattern is the connector token format from the design note:
// bsc_<integration public id>_<32 random bytes, base32>. The prefix makes the
// credential greppable by secret scanners, ours and the studio's; the id
// segment makes the broker's hashed lookup a primary-key read.
var tokenPattern = regexp.MustCompile(`\Absc_[a-z0-9][a-z0-9\-]{0,62}_[A-Za-z2-7]{32,128}\z`)

// Duration is a YAML-friendly time.Duration ("25s", "10s").
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("not a duration: %q", s)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) D() time.Duration { return time.Duration(d) }

// Config is the whole of connector.yml.
type Config struct {
	Endpoint       string   `yaml:"endpoint"`
	EndpointCAFile string   `yaml:"endpoint_ca_file"`
	Token          string   `yaml:"token"`
	TokenFile      string   `yaml:"token_file"`
	ConnectorID    string   `yaml:"connector_id"`
	LogDir         string   `yaml:"log_dir"`
	MaxConcurrent  int      `yaml:"max_concurrent"`
	Scopes         Scopes   `yaml:"scopes"`
	Toggles        Toggles  `yaml:"toggles"`
	Perforce       Perforce `yaml:"perforce"`
	TeamCity       TeamCity `yaml:"teamcity"`

	// path is where this config was loaded from; not a YAML field.
	path string
}

// Scopes are the argument-constraint lists. They live here and only here: no
// scope value is ever accepted from the wire.
type Scopes struct {
	DepotScope        []string `yaml:"depot_scope"`
	AllowedBuildTypes []string `yaml:"allowed_build_types"`
	RepoAllowlist     []string `yaml:"repo_allowlist"`
}

// Toggles are the studio's local switches. Content verbs are off in v0 at the
// schema level as well, so this switch cannot turn one on; it is here so the
// file shape does not change when they land.
type Toggles struct {
	ContentVerbs bool `yaml:"content_verbs"`
}

// Perforce is the local Helix Core connection. Note that port, user, and
// ticket are all local: no verb carries them.
type Perforce struct {
	Enabled    bool     `yaml:"enabled"`
	Binary     string   `yaml:"binary"`
	Port       string   `yaml:"port"`
	User       string   `yaml:"user"`
	Ticket     string   `yaml:"ticket"`
	TicketFile string   `yaml:"ticket_file"`
	Timeout    Duration `yaml:"timeout"`
}

// TeamCity is the local TeamCity server. allow_insecure_tls is deliberately
// scoped to this LAN server only; there is no equivalent switch for the broker
// connection, which always verifies.
type TeamCity struct {
	Enabled          bool     `yaml:"enabled"`
	URL              string   `yaml:"url"`
	Token            string   `yaml:"token"`
	TokenFile        string   `yaml:"token_file"`
	CAFile           string   `yaml:"ca_file"`
	AllowInsecureTLS bool     `yaml:"allow_insecure_tls"`
	Timeout          Duration `yaml:"timeout"`
}

// Load reads, permission-checks, and validates connector.yml.
func Load(path string) (*Config, error) {
	if err := checkSecretFileMode(path); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true) // an unrecognised key is an error, not a silent ignore
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	c.path = path
	c.applyDefaults()
	if err := c.resolveSecretFiles(); err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.MaxConcurrent == 0 {
		c.MaxConcurrent = 4
	}
	if c.Perforce.Binary == "" {
		c.Perforce.Binary = "p4"
	}
	if c.Perforce.Timeout == 0 {
		c.Perforce.Timeout = Duration(20 * time.Second)
	}
	if c.TeamCity.Timeout == 0 {
		c.TeamCity.Timeout = Duration(10 * time.Second)
	}
	if c.LogDir == "" {
		c.LogDir = filepath.Join(filepath.Dir(c.path), "logs")
	}
}

// resolveSecretFiles implements the *_file indirection. A studio with a vault
// injects a file; a studio without one writes the value inline. Either way the
// value is on the studio's disk under the studio's permissions and never
// arrives over the socket.
func (c *Config) resolveSecretFiles() error {
	pairs := []struct {
		name  string
		value *string
		file  string
	}{
		{"token", &c.Token, c.TokenFile},
		{"perforce.ticket", &c.Perforce.Ticket, c.Perforce.TicketFile},
		{"teamcity.token", &c.TeamCity.Token, c.TeamCity.TokenFile},
	}
	for _, p := range pairs {
		if p.file == "" {
			continue
		}
		if *p.value != "" {
			return fmt.Errorf("config: %s and %s_file are both set; pick one", p.name, p.name)
		}
		if err := checkSecretFileMode(p.file); err != nil {
			return err
		}
		b, err := os.ReadFile(p.file)
		if err != nil {
			return fmt.Errorf("config: %s_file: %w", p.name, err)
		}
		*p.value = strings.TrimSpace(string(b))
	}
	return nil
}

// checkSecretFileMode fails closed on a credential file any other local user
// can read. The install doc asks for 0600; this enforces it rather than hoping.
func checkSecretFileMode(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if st.IsDir() {
		return fmt.Errorf("config: %s is a directory", path)
	}
	if perm := st.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("config: %s is mode %04o; credential files must be 0600 "+
			"(no group or other access)", path, perm)
	}
	return nil
}

// ErrInsecureEndpoint is returned for anything but a verified wss:// endpoint.
var ErrInsecureEndpoint = errors.New("config: endpoint must be a wss:// URL")

// Validate is where the transport rules that matter to security live.
func (c *Config) Validate() error {
	if c.Endpoint == "" {
		return errors.New("config: endpoint is required")
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil {
		return fmt.Errorf("config: endpoint: %w", err)
	}
	if u.Scheme != "wss" {
		return fmt.Errorf("%w (got %q)", ErrInsecureEndpoint, u.Scheme)
	}
	if u.Host == "" {
		return errors.New("config: endpoint has no host")
	}
	// The token travels in a header and nowhere else. A query string on the
	// endpoint is refused at load time so that a copy-pasted ?token=... URL
	// cannot start the daemon at all -- the client half of the rule the broker
	// enforces on its side.
	if u.RawQuery != "" || strings.Contains(c.Endpoint, "?") {
		return errors.New("config: endpoint must not carry a query string; the " +
			"connector token is sent in the Authorization header only")
	}
	if u.User != nil {
		return errors.New("config: endpoint must not carry userinfo credentials")
	}
	if u.Fragment != "" {
		return errors.New("config: endpoint must not carry a fragment")
	}

	if c.Token == "" {
		return errors.New("config: token or token_file is required")
	}
	if !tokenPattern.MatchString(c.Token) {
		return errors.New("config: token is not a connector token " +
			"(expected bsc_<integration-id>_<base32 secret>)")
	}

	if c.MaxConcurrent < 1 || c.MaxConcurrent > 32 {
		return fmt.Errorf("config: max_concurrent must be 1..32, got %d", c.MaxConcurrent)
	}

	if c.Perforce.Enabled {
		if c.Perforce.Port == "" || c.Perforce.User == "" {
			return errors.New("config: perforce.enabled needs port and user")
		}
		if len(c.Scopes.DepotScope) == 0 {
			return errors.New("config: perforce.enabled needs at least one " +
				"scopes.depot_scope entry; an unscoped depot verb is denied anyway")
		}
		for i, p := range c.Scopes.DepotScope {
			if !strings.HasPrefix(p, "//") {
				return fmt.Errorf("config: depot_scope %q must begin //", p)
			}
			if strings.ContainsAny(p, "*") || strings.Contains(p, "...") {
				return fmt.Errorf("config: depot_scope %q must be a literal prefix, not a wildcard", p)
			}
			// Normalize to a trailing slash so vocab.withinPrefixList can match on
			// a real path-segment boundary: without this, a scope of "//depot/game"
			// (no trailing slash) would admit "//depot/gamesecret/..." via a bare
			// strings.HasPrefix, because "gamesecret" also starts with "game".
			if !strings.HasSuffix(p, "/") {
				c.Scopes.DepotScope[i] = p + "/"
			}
		}
	}
	if c.TeamCity.Enabled {
		if c.TeamCity.URL == "" || c.TeamCity.Token == "" {
			return errors.New("config: teamcity.enabled needs url and token (or token_file)")
		}
		tu, err := url.Parse(c.TeamCity.URL)
		if err != nil || tu.Host == "" || (tu.Scheme != "http" && tu.Scheme != "https") {
			return fmt.Errorf("config: teamcity.url must be an http(s) URL, got %q", c.TeamCity.URL)
		}
	}
	return nil
}

// Tools reports which connector.yml sections are enabled, for the vocabulary's
// tool-configuration check.
func (c *Config) Tools() map[string]bool {
	return map[string]bool{
		"perforce": c.Perforce.Enabled,
		"teamcity": c.TeamCity.Enabled,
	}
}

// IntegrationID is the middle segment of the connector token. It identifies the
// integration to the broker; the broker re-derives it from the authenticated
// session rather than trusting this, but the connector reports it in hello.
func (c *Config) IntegrationID() string {
	parts := strings.Split(c.Token, "_")
	if len(parts) < 3 {
		return ""
	}
	return parts[1]
}

// Redacted renders the config for the log with every credential removed. The
// audit log and the startup banner both use this; no code path prints Config
// directly.
func (c *Config) Redacted() string {
	var b strings.Builder
	fmt.Fprintf(&b, "endpoint=%s connector_id=%s integration_id=%s max_concurrent=%d\n",
		c.Endpoint, c.ConnectorID, c.IntegrationID(), c.MaxConcurrent)
	fmt.Fprintf(&b, "token=bsc_%s_[redacted] log_dir=%s\n", c.IntegrationID(), c.LogDir)
	fmt.Fprintf(&b, "scopes.depot_scope=%v allowed_build_types=%v repo_allowlist=%v\n",
		c.Scopes.DepotScope, c.Scopes.AllowedBuildTypes, c.Scopes.RepoAllowlist)
	fmt.Fprintf(&b, "perforce.enabled=%t port=%s user=%s ticket=[redacted]\n",
		c.Perforce.Enabled, c.Perforce.Port, c.Perforce.User)
	fmt.Fprintf(&b, "teamcity.enabled=%t url=%s token=[redacted]",
		c.TeamCity.Enabled, c.TeamCity.URL)
	return b.String()
}

// Path returns where this config was loaded from.
func (c *Config) Path() string { return c.path }
