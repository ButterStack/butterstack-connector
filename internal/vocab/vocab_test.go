package vocab

import (
	"encoding/json"
	"strings"
	"testing"
)

var testScopes = Scopes{
	DepotScope:        []string{"//butterstack-uat/", "//depot/game/"},
	AllowedBuildTypes: []string{"Uat_Build"},
	RepoAllowlist:     []string{"studio/game"},
}

var allTools = ConfiguredTools{"teamcity": true, "perforce": true, "jenkins": true, "ghes": true, "horde": true}

func TestSelfcheckPasses(t *testing.T) {
	if err := Selfcheck(); err != nil {
		t.Fatalf("vocabulary selfcheck failed: %v", err)
	}
}

// TestNoVerbAcceptsABannedArgument is the regression guard for Shuri F4. A
// caller-supplied parameter bag on a build-triggering verb is the finding that
// falsified the bounded-blast-radius claim; re-adding one must fail the build,
// not ship.
func TestNoVerbAcceptsABannedArgument(t *testing.T) {
	banned := BannedArgNames()
	for i := range Vocabulary {
		v := &Vocabulary[i]
		for j := range v.Args {
			name := strings.ToLower(v.Args[j].Name)
			if why, bad := banned[name]; bad {
				t.Errorf("%s declares banned argument %q (%s)", v.Name, name, why)
			}
		}
	}
}

// TestEveryArgumentIsScalar keeps the schema incapable of expressing a map.
func TestEveryArgumentIsScalar(t *testing.T) {
	for i := range Vocabulary {
		v := &Vocabulary[i]
		for j := range v.Args {
			switch v.Args[j].Kind {
			case KindInt, KindBool, KindString, KindDepotPath:
			default:
				t.Errorf("%s.%s has non-scalar kind %q", v.Name, v.Args[j].Name, v.Args[j].Kind)
			}
		}
	}
}

// TestNoMutatingOrContentVerbIsCompiled is the v0 posture: the spike runs with
// every X-class and C-class verb off.
func TestNoMutatingOrContentVerbIsCompiled(t *testing.T) {
	for i := range Vocabulary {
		v := &Vocabulary[i]
		if v.Compiled && v.Mutating {
			t.Errorf("%s is mutating and compiled", v.Name)
		}
		if v.Compiled && v.Class == ClassContent {
			t.Errorf("%s is content class and compiled", v.Name)
		}
	}
}

func TestNoSysExec(t *testing.T) {
	for _, name := range []string{"sys.exec", "sys.shell", "sys.run", "exec"} {
		if Lookup(name) != nil {
			t.Fatalf("%s exists in the vocabulary", name)
		}
	}
}

func resolve(t *testing.T, verb, args string) (*Verb, map[string]any, *DenyError) {
	t.Helper()
	return Resolve(verb, json.RawMessage(args), testScopes, allTools)
}

func TestOutOfVocabularyVerbIsDenied(t *testing.T) {
	for _, verb := range []string{"sys.exec", "p4.print", "teamcity.build.delete", ""} {
		_, _, derr := resolve(t, verb, `{}`)
		if derr == nil || derr.Reason != ReasonUnknownVerb {
			t.Errorf("%q: want %s, got %v", verb, ReasonUnknownVerb, derr)
		}
	}
}

func TestReservedVerbIsDenied(t *testing.T) {
	for _, verb := range []string{"jenkins.build.trigger", "teamcity.build.queue", "horde.server.info"} {
		_, _, derr := resolve(t, verb, `{}`)
		if derr == nil || derr.Reason != ReasonVerbNotCompiled {
			t.Errorf("%q: want %s, got %v", verb, ReasonVerbNotCompiled, derr)
		}
	}
}

func TestContentVerbIsDeniedEvenWhenNamed(t *testing.T) {
	_, _, derr := resolve(t, "p4.file_contents", `{"depot_path":"//depot/game/x.uasset"}`)
	if derr == nil || derr.Reason != ReasonVerbNotCompiled {
		t.Fatalf("want %s, got %v", ReasonVerbNotCompiled, derr)
	}
}

// TestSmuggledParamsAreRefused: the drill that actually tests condition 2 for
// the trigger verbs. Even if a trigger verb were compiled in, an unknown key
// stops the command instead of being ignored into the tool call.
func TestSmuggledParamsAreRefused(t *testing.T) {
	cases := []struct{ verb, args string }{
		{"teamcity.build.get", `{"build_id":42,"properties":{"X":"$(id)"}}`},
		{"teamcity.build.get", `{"build_id":42,"params":{"X":"1"}}`},
		{"p4.describe", `{"change":7,"command":"rm -rf /"}`},
		{"p4.changes", `{"path":"//depot/game/...","url":"http://evil"}`},
		{"sys.ping", `{"host":"10.0.0.1"}`},
	}
	for _, c := range cases {
		_, _, derr := resolve(t, c.verb, c.args)
		if derr == nil || derr.Reason != ReasonUnknownArgument {
			t.Errorf("%s %s: want %s, got %v", c.verb, c.args, ReasonUnknownArgument, derr)
		}
	}
}

func TestArgumentTypeValidationAtTheFrameBoundary(t *testing.T) {
	cases := []struct{ verb, args, reason string }{
		{"teamcity.build.get", `{"build_id":"42"}`, ReasonArgumentType},
		{"teamcity.build.get", `{"build_id":1.5}`, ReasonArgumentType},
		{"teamcity.build.get", `{"build_id":0}`, ReasonArgumentRange},
		{"teamcity.build.get", `{}`, ReasonMissingArgument},
		{"teamcity.build.get", `[1,2]`, ReasonMalformedArgs},
		{"p4.describe", `{"change":7,"max_files":100000}`, ReasonArgumentRange},
		{"p4.changes", `{"path":"//depot/game/...","max":9999}`, ReasonArgumentRange},
		{"p4.changes", `{"path":42}`, ReasonArgumentType},
	}
	for _, c := range cases {
		_, _, derr := resolve(t, c.verb, c.args)
		if derr == nil || derr.Reason != c.reason {
			t.Errorf("%s %s: want %s, got %v", c.verb, c.args, c.reason, derr)
		}
	}
}

// TestOutOfScopeArgumentIsDenied is the drill Shuri singled out: an
// in-vocabulary, well-typed verb whose argument falls outside the studio's own
// scope list is refused exactly like an unknown verb.
func TestOutOfScopeArgumentIsDenied(t *testing.T) {
	cases := []struct{ args, reason string }{
		{`{"path":"//..."}`, ReasonOutOfScopePath},
		{`{"path":"//*"}`, ReasonOutOfScopePath},
		{`{"path":"//other/secret/..."}`, ReasonOutOfScopePath},
		{`{"path":"//depot/..."}`, ReasonOutOfScopePath},
		{`{"path":"//depot/game/../../other/..."}`, ReasonArgumentPattern},
		{`{"path":"//depot/game/x@=123"}`, ReasonArgumentPattern},
		{`{"path":"depot/game/..."}`, ReasonArgumentPattern},
	}
	for _, c := range cases {
		_, _, derr := resolve(t, "p4.changes", c.args)
		if derr == nil || derr.Reason != c.reason {
			t.Errorf("p4.changes %s: want %s, got %v", c.args, c.reason, derr)
		}
	}
}

func TestInScopePathIsAllowed(t *testing.T) {
	for _, p := range []string{
		`//depot/game/...`,
		`//depot/game/Content/*`,
		`//butterstack-uat/main/...`,
		`//depot/game/Content/Maps/Main.umap`,
	} {
		_, args, derr := resolve(t, "p4.changes", `{"path":"`+p+`"}`)
		if derr != nil {
			t.Fatalf("%s: unexpected deny %v", p, derr)
		}
		if args["path"] != p {
			t.Fatalf("%s: argument not passed through", p)
		}
	}
}

// TestDepotScopeRespectsSegmentBoundary is the regression guard for the
// bare-strings.HasPrefix bug: a scope of "//depot/game/" must not admit
// "//depot/gamesecret/...", because "gamesecret" also starts with the
// literal string "game". Only a real path-segment boundary (the scope root
// itself, or the scope as a genuine "prefix/" match) counts as in-scope.
func TestDepotScopeRespectsSegmentBoundary(t *testing.T) {
	cases := []struct {
		path    string
		allowed bool
	}{
		{`//depot/gamesecret/...`, false}, // segment-boundary bypass: denied
		{`//depot/game/...`, true},        // genuine child of the scope: allowed
		{`//depot/game`, true},            // the scope root itself, exact: allowed
	}
	for _, c := range cases {
		_, _, derr := resolve(t, "p4.changes", `{"path":"`+c.path+`"}`)
		if c.allowed && derr != nil {
			t.Errorf("%s: want allowed, got deny %v", c.path, derr)
		}
		if !c.allowed && (derr == nil || derr.Reason != ReasonOutOfScopePath) {
			t.Errorf("%s: want %s, got %v", c.path, ReasonOutOfScopePath, derr)
		}
	}
}

// TestWithinPrefixList unit-tests the matcher directly: a scope entry without
// a trailing slash is skipped rather than matched with a bare prefix check.
func TestWithinPrefixList(t *testing.T) {
	scopes := []string{"//depot/game/"}
	cases := map[string]bool{
		"//depot/gamesecret/...": false,
		"//depot/game/...":       true,
		"//depot/game":           true,
		"//depot/gam":            false,
	}
	for path, want := range cases {
		if got := withinPrefixList(literalPrefix(path), scopes); got != want {
			t.Errorf("withinPrefixList(%q) = %v, want %v", path, got, want)
		}
	}

	// An un-normalized scope entry (no trailing slash) is skipped entirely:
	// config.Validate is what normalizes entries, and this function must not
	// silently fall back to the unsafe bare-prefix match if it somehow sees
	// one that wasn't.
	if withinPrefixList("//depot/game", []string{"//depot/game"}) {
		t.Fatalf("an un-normalized scope entry (no trailing slash) must not match")
	}
}

// TestContentToggleCannotBeTurnedOnByACaller pins include_diff to false at the
// schema level, so no broker frame and no config value can flip it in v0.
func TestContentToggleCannotBeTurnedOnByACaller(t *testing.T) {
	_, _, derr := resolve(t, "p4.describe", `{"change":7,"include_diff":true}`)
	if derr == nil || derr.Reason != ReasonContentVerbOff {
		t.Fatalf("want %s, got %v", ReasonContentVerbOff, derr)
	}
	if _, _, derr := resolve(t, "p4.describe", `{"change":7,"include_diff":false}`); derr != nil {
		t.Fatalf("include_diff:false should be accepted, got %v", derr)
	}
}

func TestUnconfiguredToolIsDeniedButVocabularyIsCheckedFirst(t *testing.T) {
	none := ConfiguredTools{}
	// A configured-tool denial must never be reachable for a name that is not
	// in the vocabulary: an unknown verb must not reveal what is configured.
	if _, _, derr := Resolve("p4.nope", json.RawMessage(`{}`), testScopes, none); derr.Reason != ReasonUnknownVerb {
		t.Fatalf("want %s, got %s", ReasonUnknownVerb, derr.Reason)
	}
	if _, _, derr := Resolve("p4.changes", json.RawMessage(`{"path":"//depot/game/..."}`), testScopes, none); derr.Reason != ReasonToolNotConfigured {
		t.Fatalf("want %s, got %s", ReasonToolNotConfigured, derr.Reason)
	}
}

func TestEmptyDepotScopeDeniesEveryPath(t *testing.T) {
	_, _, derr := Resolve("p4.changes", json.RawMessage(`{"path":"//depot/game/..."}`), Scopes{}, allTools)
	if derr == nil || derr.Reason != ReasonOutOfScopePath {
		t.Fatalf("an unconfigured depot_scope must fail closed, got %v", derr)
	}
}

func TestLiteralPrefix(t *testing.T) {
	cases := map[string]string{
		"//...":                  "//",
		"//depot/game/...":       "//depot/game/",
		"//depot/game/*":         "//depot/game/",
		"//depot/a.b/...":        "//depot/a.b/",
		"//depot/game/Main.umap": "//depot/game/Main.umap",
		"//depot/game/*.uasset":  "//depot/game/",
	}
	for in, want := range cases {
		if got := literalPrefix(in); got != want {
			t.Errorf("literalPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCompiledVerbsAreTheAnnouncedCapabilities(t *testing.T) {
	got := CompiledVerbs()
	want := map[string]bool{
		"sys.ping": true, "sys.version": true, "sys.capabilities": true,
		"teamcity.server.info": true, "teamcity.build.get": true,
		"p4.describe": true, "p4.changes": true,
	}
	if len(got) != len(want) {
		t.Fatalf("compiled verbs = %v", got)
	}
	for _, v := range got {
		if !want[v] {
			t.Errorf("unexpected compiled verb %q", v)
		}
	}
}
