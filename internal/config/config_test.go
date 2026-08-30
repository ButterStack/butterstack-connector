package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const goodToken = "bsc_intg7f3a_MFRGGZDFMZTWQ2LKNNWG23TPOBYXE43U"

func writeCfg(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "connector.yml")
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func base(extra string) string {
	return `endpoint: wss://staging.butterstack.com/connect
connector_id: home-lab-1
token: ` + goodToken + `
` + extra
}

func TestLoadsAValidConfig(t *testing.T) {
	p := writeCfg(t, base(`scopes:
  depot_scope:
    - //depot/game/
teamcity:
  enabled: true
  url: http://localhost:8111
  token: tc-token
`), 0o600)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.IntegrationID() != "intg7f3a" {
		t.Fatalf("integration id = %q", c.IntegrationID())
	}
	if !c.Tools()["teamcity"] || c.Tools()["perforce"] {
		t.Fatalf("tools = %v", c.Tools())
	}
}

// TestWorldReadableConfigIsRefused: condition 3 says the studio's credentials
// live in a 0600 file on the studio's host. This enforces it instead of
// documenting it.
func TestWorldReadableConfigIsRefused(t *testing.T) {
	p := writeCfg(t, base(""), 0o644)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("want a permission refusal, got %v", err)
	}
}

// TestQueryStringEndpointIsRefused is drill (g)'s client half: a copy-pasted
// ?token= URL cannot start the daemon at all.
func TestQueryStringEndpointIsRefused(t *testing.T) {
	p := writeCfg(t, `endpoint: wss://staging.butterstack.com/connect?token=`+goodToken+`
token: `+goodToken+`
`, 0o600)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "query string") {
		t.Fatalf("want a query-string refusal, got %v", err)
	}
}

func TestPlaintextAndUserinfoEndpointsAreRefused(t *testing.T) {
	for _, ep := range []string{
		"ws://staging.butterstack.com/connect",
		"https://staging.butterstack.com/connect",
		"wss://user:pass@staging.butterstack.com/connect",
	} {
		p := writeCfg(t, "endpoint: "+ep+"\ntoken: "+goodToken+"\n", 0o600)
		if _, err := Load(p); err == nil {
			t.Errorf("%s was accepted", ep)
		}
	}
}

func TestMalformedTokenIsRefused(t *testing.T) {
	for _, tok := range []string{"hunter2", "bsc_short", "bsc_intg_abc", "Bearer bsc_a_MFRGGZDFMZTWQ2LKNNWG23TPOBYXE43U"} {
		p := writeCfg(t, "endpoint: wss://x.example/connect\ntoken: "+tok+"\n", 0o600)
		if _, err := Load(p); err == nil {
			t.Errorf("token %q was accepted", tok)
		}
	}
}

// TestNoEnvironmentCredentialFallback pins the custody rule: the only source of
// a credential is the file the studio wrote.
func TestNoEnvironmentCredentialFallback(t *testing.T) {
	t.Setenv("BUTTERSTACK_CONNECTOR_TOKEN", goodToken)
	t.Setenv("CONNECTOR_TOKEN", goodToken)
	p := writeCfg(t, "endpoint: wss://x.example/connect\n", 0o600)
	if _, err := Load(p); err == nil {
		t.Fatal("a config with no token loaded; an environment variable must not fill it in")
	}
}

func TestSecretFileIndirection(t *testing.T) {
	dir := t.TempDir()
	tf := filepath.Join(dir, "token")
	if err := os.WriteFile(tf, []byte(goodToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "connector.yml")
	if err := os.WriteFile(p, []byte("endpoint: wss://x.example/connect\ntoken_file: "+tf+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Token != goodToken {
		t.Fatalf("token not read from token_file")
	}

	// A world-readable secret file is refused for the same reason as the config.
	if err := os.Chmod(tf, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("a 0644 token_file was accepted")
	}
}

func TestUnknownKeyIsAnError(t *testing.T) {
	p := writeCfg(t, base("tunnel: true\n"), 0o600)
	if _, err := Load(p); err == nil {
		t.Fatal("an unrecognised connector.yml key was ignored instead of refused")
	}
}

func TestPerforceNeedsADepotScope(t *testing.T) {
	p := writeCfg(t, base(`perforce:
  enabled: true
  port: ssl:p4.lan:1666
  user: butterstack-ro
`), 0o600)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "depot_scope") {
		t.Fatalf("want a depot_scope requirement, got %v", err)
	}
}

func TestWildcardDepotScopeIsRefused(t *testing.T) {
	p := writeCfg(t, base(`scopes:
  depot_scope:
    - //...
perforce:
  enabled: true
  port: ssl:p4.lan:1666
  user: butterstack-ro
`), 0o600)
	if _, err := Load(p); err == nil {
		t.Fatal("a wildcard depot_scope was accepted")
	}
}

// TestRedactedNeverPrintsASecret guards the audit log and the startup banner.
func TestRedactedNeverPrintsASecret(t *testing.T) {
	p := writeCfg(t, base(`teamcity:
  enabled: true
  url: http://localhost:8111
  token: TEAMCITY-SUPER-SECRET
perforce:
  enabled: true
  port: ssl:p4.lan:1666
  user: butterstack-ro
  ticket: P4-TICKET-SECRET
scopes:
  depot_scope:
    - //depot/game/
`), 0o600)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	out := c.Redacted()
	for _, secret := range []string{"TEAMCITY-SUPER-SECRET", "P4-TICKET-SECRET", "MFRGGZDFMZTWQ2LKNNWG23TPOBYXE43U"} {
		if strings.Contains(out, secret) {
			t.Errorf("Redacted() leaked %q:\n%s", secret, out)
		}
	}
}
