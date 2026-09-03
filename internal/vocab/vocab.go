// Package vocab is the typed command allowlist and the argument-constraint
// layer. It is the whole of survival condition 2 ("typed allowlist with
// constrained arguments, never a tunnel or shell") in one readable file, which
// is the point: an IT director is asked to read the source, so the source has
// to be readable.
//
// Two layers, both of which must pass before any tool is touched:
//
//  1. The verb must be in the compiled vocabulary. A verb that is not compiled
//     in -- including a name this schema reserves for a later version -- is
//     denied. There is no dynamic registration and no sys.exec.
//
//  2. Every argument is validated against a fixed per-verb schema before the
//     tool call: unknown keys are refused outright (this is what stops a
//     smuggled params map, Shuri F4), scalars are type- and range-checked, and
//     path/id arguments are matched against scopes that live only in the
//     studio's connector.yml (Shuri F5).
//
// A well-formed verb with an out-of-scope argument is denied exactly like an
// unknown verb, and both write a local audit line. Only the second kind of
// denial actually tests condition 2, which is why the drills assert both.
package vocab

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Class is the egress class of a verb's output, per Devin design note 2.4.
// M = metadata, P = paths, C = content, X = mutation.
type Class string

const (
	ClassMetadata Class = "M"
	ClassPaths    Class = "P"
	ClassContent  Class = "C"
	ClassMutation Class = "X"
)

// Kind is the type of a single argument. Every kind is a scalar. There is
// deliberately no map, object, or free-form kind: a verb that could carry an
// arbitrary key/value bag would be a code-execution primitive on the studio's
// build agents the moment those keys reach a build step, which is the finding
// that made this layer day-1 work rather than v1 polish.
type Kind string

const (
	KindInt       Kind = "int"
	KindBool      Kind = "bool"
	KindString    Kind = "string"
	KindDepotPath Kind = "depot_path"
)

// Scope names a constraint list that lives in connector.yml, never on the wire.
type Scope string

const (
	ScopeNone            Scope = ""
	ScopeDepot           Scope = "depot_scope"
	ScopeAllowedBuildTys Scope = "allowed_build_types"
	ScopeRepoAllowlist   Scope = "repo_allowlist"
)

// Arg is one argument's schema.
type Arg struct {
	Name     string
	Kind     Kind
	Required bool

	// Int constraints.
	Min int64
	Max int64

	// String constraints.
	Pattern *regexp.Regexp
	MaxLen  int

	// Bool constraints. MustBeFalse encodes a v0 toggle that is off at the
	// schema level, not just in config: include_diff cannot be turned on by a
	// caller no matter what the broker sends.
	MustBeFalse bool

	// Scope, if any, that the value must fall inside.
	Scope Scope

	Doc string
}

// Verb is one entry in the vocabulary.
type Verb struct {
	Name  string
	Class Class

	// Compiled reports whether this build can actually execute the verb.
	// Reserved names are listed with Compiled=false so that the vocabulary is
	// self-documenting and so that a reserved name's (empty) argument schema is
	// covered by the same tests as a live one -- in particular the test that no
	// verb anywhere accepts caller-supplied build parameters.
	Compiled bool

	// Mutating marks X-class verbs. No X-class verb is compiled in v0.
	Mutating bool

	Args []Arg

	// DefaultMaxBytes caps the result body when the broker does not say.
	DefaultMaxBytes int

	// Tool names the connector.yml section this verb needs configured.
	Tool string

	Doc string
}

// Scopes are the studio-supplied constraint lists. Empty means "nothing is in
// scope" for the depot list -- fail closed -- while an empty build-type or repo
// list means the same. A studio that configures no depot_scope cannot run a
// path verb, which is the correct default for a daemon inside someone's LAN.
type Scopes struct {
	DepotScope        []string
	AllowedBuildTypes []string
	RepoAllowlist     []string
}

// DenyReason values are stable machine tokens. The drills assert on them and
// the audit log stores them, so they are part of the protocol surface.
const (
	ReasonUnknownVerb        = "unknown_verb"
	ReasonVerbNotCompiled    = "verb_not_compiled"
	ReasonToolNotConfigured  = "tool_not_configured"
	ReasonMalformedArgs      = "malformed_args"
	ReasonUnknownArgument    = "unknown_argument"
	ReasonMissingArgument    = "missing_argument"
	ReasonArgumentType       = "argument_type"
	ReasonArgumentRange      = "argument_range"
	ReasonArgumentPattern    = "argument_pattern"
	ReasonContentVerbOff     = "content_verb_disabled"
	ReasonOutOfScopePath     = "out_of_scope_path"
	ReasonOutOfScopeBuildTy  = "out_of_scope_build_type"
	ReasonOutOfScopeRepo     = "out_of_scope_repo"
	ReasonDuplicateCommandID = "duplicate_command_id"
	ReasonDeadlineExceeded   = "deadline_exceeded"
)

// DenyError carries a stable reason plus a human detail. The detail is written
// to the local audit log only; it never leaves the studio network, because a
// denial message that echoed the rejected value back would be a small egress
// channel of its own.
type DenyError struct {
	Reason string
	Detail string
}

func (e *DenyError) Error() string {
	if e.Detail == "" {
		return e.Reason
	}
	return e.Reason + ": " + e.Detail
}

func deny(reason, format string, a ...any) *DenyError {
	return &DenyError{Reason: reason, Detail: fmt.Sprintf(format, a...)}
}

// bannedArgNames are argument names that must never appear in any verb's
// schema, compiled or reserved. Each one is a documented path from "the broker
// asked for a build" to "the broker ran a command on a studio build agent":
// Jenkins parameters and TeamCity properties are interpolated into build steps
// by design. The vocabulary test enforces this list, so re-adding one of these
// names fails the build rather than shipping.
var bannedArgNames = map[string]string{
	"params":      "Jenkins build parameters interpolate into shell build steps",
	"parameters":  "same as params",
	"properties":  "TeamCity properties are consumed by build steps",
	"env":         "environment injection reaches build steps",
	"branchname":  "branch names reach VCS checkout logic and build steps",
	"branch_name": "branch names reach VCS checkout logic and build steps",
	"comment":     "free text on a queue request, no v0 need",
	"url":         "a verb that takes a URL is a tunnel",
	"host":        "a verb that takes a host is a tunnel",
	"port":        "a verb that takes a port is a tunnel",
	"command":     "a verb that takes a command is a shell",
	"script":      "a verb that takes a script is a shell",
	"args":        "a verb that takes an argv is a shell",
}

var (
	reBuildTypeID = regexp.MustCompile(`\A[A-Za-z0-9_]{1,190}\z`)
	reHexSHA      = regexp.MustCompile(`\A[0-9a-f]{7,64}\z`)
)

// Vocabulary is the whole allowlist for protocol v0. Compiled verbs first,
// then reserved names. Adding a verb here is the only way to add one: there is
// no plugin path and nothing reads a verb name from config.
var Vocabulary = []Verb{
	// ---- connector-internal -------------------------------------------------
	{
		Name: "sys.ping", Class: ClassMetadata, Compiled: true, DefaultMaxBytes: 1024,
		Doc: "liveness round trip; touches no studio tool",
	},
	{
		Name: "sys.version", Class: ClassMetadata, Compiled: true, DefaultMaxBytes: 1024,
		Doc: "connector build version and protocol version",
	},
	{
		Name: "sys.capabilities", Class: ClassMetadata, Compiled: true, DefaultMaxBytes: 8192,
		Doc: "the compiled verb list and which tools are configured",
	},

	// ---- TeamCity -----------------------------------------------------------
	{
		Name: "teamcity.server.info", Class: ClassMetadata, Compiled: true, Tool: "teamcity",
		DefaultMaxBytes: 8192,
		Doc:             "GET /app/rest/server; the test_connection analog",
	},
	{
		Name: "teamcity.build.get", Class: ClassMetadata, Compiled: true, Tool: "teamcity",
		DefaultMaxBytes: 32768,
		Args: []Arg{
			{Name: "build_id", Kind: KindInt, Required: true, Min: 1, Max: 1 << 40,
				Doc: "TeamCity build id, integer-validated at the frame boundary"},
		},
		Doc: "GET /app/rest/builds/id:<id> with a fixed fields= projection",
	},
	{
		Name: "teamcity.build.queue", Class: ClassMutation, Mutating: true, Compiled: false,
		Tool: "teamcity",
		Args: []Arg{
			{Name: "build_type_id", Kind: KindString, Required: true, Pattern: reBuildTypeID,
				MaxLen: 190, Scope: ScopeAllowedBuildTys,
				Doc: "must appear in allowed_build_types in connector.yml"},
		},
		Doc: "RESERVED, not compiled in v0. When it ships, the connector composes a " +
			"fixed request body {\"buildType\":{\"id\":...}}; there is no argument here " +
			"for properties, branchName, or comment, and there never will be at v0.",
	},

	// ---- Perforce -----------------------------------------------------------
	{
		Name: "p4.describe", Class: ClassPaths, Compiled: true, Tool: "perforce",
		DefaultMaxBytes: 65536,
		Args: []Arg{
			{Name: "change", Kind: KindInt, Required: true, Min: 1, Max: 1 << 40,
				Doc: "changelist number, integer-validated before the p4 call"},
			{Name: "max_files", Kind: KindInt, Min: 1, Max: 1000,
				Doc: "cap on the returned file list"},
			{Name: "include_diff", Kind: KindBool, MustBeFalse: true,
				Doc: "content class; not available in v0 at any config setting"},
		},
		Doc: "p4 describe -s <change>, invoked as an argv array with no shell",
	},
	{
		Name: "p4.changes", Class: ClassPaths, Compiled: true, Tool: "perforce",
		DefaultMaxBytes: 65536,
		Args: []Arg{
			{Name: "path", Kind: KindDepotPath, Required: true, Scope: ScopeDepot, MaxLen: 1024,
				Doc: "depot path; prefix-matched against depot_scope, no wildcard above the prefix"},
			{Name: "max", Kind: KindInt, Min: 1, Max: 200,
				Doc: "cap on the number of changelists returned"},
		},
		Doc: "p4 changes -m <max> <path>; the path-scoped verb the out-of-scope drill exercises",
	},
	{
		Name: "p4.file_contents", Class: ClassContent, Compiled: false, Tool: "perforce",
		Args: []Arg{
			{Name: "depot_path", Kind: KindDepotPath, Required: true, Scope: ScopeDepot, MaxLen: 1024},
			{Name: "rev", Kind: KindInt, Min: 1, Max: 1 << 32},
			{Name: "max_bytes", Kind: KindInt, Min: 1, Max: 1 << 20},
		},
		Doc: "RESERVED, not compiled in v0. Content class; the spike runs with every " +
			"content verb off, so this name exists to be denied.",
	},

	// ---- Jenkins ------------------------------------------------------------
	{
		Name: "jenkins.build.trigger", Class: ClassMutation, Mutating: true, Compiled: false,
		Tool: "jenkins",
		Args: []Arg{
			{Name: "job", Kind: KindString, Required: true, MaxLen: 190,
				Pattern: regexp.MustCompile(`\A[A-Za-z0-9._\-/]{1,190}\z`),
				Doc:     "must appear in allowed_jobs in connector.yml"},
		},
		Doc: "RESERVED, not compiled in v0. When it ships it triggers with the job's own " +
			"default parameters. There is no params argument: Jenkins parameters " +
			"interpolate into shell build steps, which would make the allowlist a " +
			"code-execution primitive and falsify the bounded-blast-radius claim.",
	},

	// ---- GitHub Enterprise Server -------------------------------------------
	{
		Name: "ghes.commit.get", Class: ClassPaths, Compiled: false, Tool: "ghes",
		Args: []Arg{
			{Name: "repo", Kind: KindString, Required: true, Scope: ScopeRepoAllowlist, MaxLen: 190,
				Pattern: regexp.MustCompile(`\A[A-Za-z0-9._\-]{1,100}/[A-Za-z0-9._\-]{1,100}\z`)},
			{Name: "sha", Kind: KindString, Required: true, Pattern: reHexSHA, MaxLen: 64},
		},
		Doc: "RESERVED, not compiled in v0. Listed so repo_allowlist has a schema to bind to.",
	},

	// ---- Horde --------------------------------------------------------------
	{
		Name: "horde.server.info", Class: ClassMetadata, Compiled: false, Tool: "horde",
		Doc: "RESERVED, not compiled in v0. Horde is connector-only and has no push path.",
	},
}

// index is the compiled lookup table, built once.
var index = func() map[string]*Verb {
	m := make(map[string]*Verb, len(Vocabulary))
	for i := range Vocabulary {
		if _, dup := m[Vocabulary[i].Name]; dup {
			panic("vocab: duplicate verb " + Vocabulary[i].Name)
		}
		m[Vocabulary[i].Name] = &Vocabulary[i]
	}
	return m
}()

// Lookup returns the verb schema, or nil when the name is not in the
// vocabulary at all.
func Lookup(name string) *Verb { return index[name] }

// CompiledVerbs is the capability list announced in hello, sorted for stable
// comparison in tests and audit output.
func CompiledVerbs() []string {
	out := make([]string, 0, len(Vocabulary))
	for i := range Vocabulary {
		if Vocabulary[i].Compiled {
			out = append(out, Vocabulary[i].Name)
		}
	}
	sort.Strings(out)
	return out
}

// ConfiguredTools is the set of connector.yml sections that are enabled.
type ConfiguredTools map[string]bool

// Resolve applies both layers and returns the validated argument map.
//
// The order matters and is asserted by the tests: vocabulary membership, then
// compiled-in, then tool configuration, then argument validation. A name that
// is not in the vocabulary must never reveal whether a tool is configured.
func Resolve(name string, rawArgs json.RawMessage, scopes Scopes, tools ConfiguredTools) (*Verb, map[string]any, *DenyError) {
	v := Lookup(name)
	if v == nil {
		return nil, nil, deny(ReasonUnknownVerb, "%q is not in the vocabulary", name)
	}
	if !v.Compiled {
		return v, nil, deny(ReasonVerbNotCompiled, "%q is reserved but not compiled into this build", name)
	}
	if v.Class == ClassContent {
		// Belt and braces: no content-class verb is compiled in v0, and if one
		// ever is, it still cannot run without an explicit local toggle that
		// this build does not read. Fail closed.
		return v, nil, deny(ReasonContentVerbOff, "%q is a content-class verb", name)
	}
	if v.Tool != "" && !tools[v.Tool] {
		return v, nil, deny(ReasonToolNotConfigured, "no %s section in connector.yml", v.Tool)
	}
	args, derr := v.validateArgs(rawArgs, scopes)
	if derr != nil {
		return v, nil, derr
	}
	return v, args, nil
}

// validateArgs enforces the per-verb argument schema.
func (v *Verb) validateArgs(raw json.RawMessage, scopes Scopes) (map[string]any, *DenyError) {
	supplied := map[string]json.RawMessage{}
	if len(raw) > 0 && string(raw) != "null" {
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.UseNumber()
		if err := dec.Decode(&supplied); err != nil {
			return nil, deny(ReasonMalformedArgs, "args is not a JSON object: %v", err)
		}
	}

	// Unknown keys are refused before anything else is looked at. This single
	// rule is what stops a smuggled parameter bag: an argument the schema does
	// not name cannot be ignored into the tool call, it stops the command.
	known := make(map[string]*Arg, len(v.Args))
	for i := range v.Args {
		known[v.Args[i].Name] = &v.Args[i]
	}
	for k := range supplied {
		if _, ok := known[k]; !ok {
			return nil, deny(ReasonUnknownArgument, "%s does not accept %q", v.Name, k)
		}
	}

	out := make(map[string]any, len(v.Args))
	for i := range v.Args {
		a := &v.Args[i]
		rawVal, present := supplied[a.Name]
		if !present {
			if a.Required {
				return nil, deny(ReasonMissingArgument, "%s requires %q", v.Name, a.Name)
			}
			continue
		}
		val, derr := a.validate(rawVal, scopes)
		if derr != nil {
			return nil, derr
		}
		out[a.Name] = val
	}
	return out, nil
}

func (a *Arg) validate(raw json.RawMessage, scopes Scopes) (any, *DenyError) {
	switch a.Kind {
	case KindInt:
		// encoding/json will happily decode the JSON *string* "42" into a
		// json.Number, because json.Number is a string type and "42" is a valid
		// number literal. That would let a caller supply a quoted value where an
		// integer is required, so the JSON type is checked first and only then
		// the range: an argument's declared type is part of the allowlist.
		var any0 any
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.UseNumber()
		if err := dec.Decode(&any0); err != nil {
			return nil, deny(ReasonArgumentType, "%s must be an integer", a.Name)
		}
		n, ok := any0.(json.Number)
		if !ok {
			return nil, deny(ReasonArgumentType, "%s must be an integer", a.Name)
		}
		i, err := n.Int64()
		if err != nil {
			return nil, deny(ReasonArgumentType, "%s must be an integer, not %s", a.Name, n.String())
		}
		if i < a.Min || i > a.Max {
			return nil, deny(ReasonArgumentRange, "%s must be between %d and %d", a.Name, a.Min, a.Max)
		}
		return i, nil

	case KindBool:
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, deny(ReasonArgumentType, "%s must be a boolean", a.Name)
		}
		if a.MustBeFalse && b {
			return nil, deny(ReasonContentVerbOff, "%s cannot be enabled in protocol v0", a.Name)
		}
		return b, nil

	case KindString, KindDepotPath:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, deny(ReasonArgumentType, "%s must be a string", a.Name)
		}
		if a.MaxLen > 0 && len(s) > a.MaxLen {
			return nil, deny(ReasonArgumentRange, "%s exceeds %d bytes", a.Name, a.MaxLen)
		}
		if strings.ContainsAny(s, "\x00\n\r") {
			return nil, deny(ReasonArgumentPattern, "%s contains a control character", a.Name)
		}
		if a.Kind == KindDepotPath {
			if derr := validateDepotPath(a.Name, s); derr != nil {
				return nil, derr
			}
		}
		if a.Pattern != nil && !a.Pattern.MatchString(s) {
			return nil, deny(ReasonArgumentPattern, "%s does not match the required form", a.Name)
		}
		if derr := a.checkScope(s, scopes); derr != nil {
			return nil, derr
		}
		return s, nil
	}
	return nil, deny(ReasonArgumentType, "%s has no validator", a.Name)
}

// validateDepotPath applies Perforce-specific syntax rules before scope is even
// considered. Revision specifiers are rejected because they change what the
// path means, and traversal segments because a scope prefix that can be escaped
// is not a scope.
func validateDepotPath(name, s string) *DenyError {
	if !strings.HasPrefix(s, "//") {
		return deny(ReasonArgumentPattern, "%s must be a depot path beginning //", name)
	}
	if strings.ContainsAny(s, "@#%") {
		return deny(ReasonArgumentPattern, "%s may not carry a revision specifier", name)
	}
	for _, seg := range strings.Split(strings.TrimPrefix(s, "//"), "/") {
		if seg == ".." || seg == "." {
			return deny(ReasonArgumentPattern, "%s may not contain a traversal segment", name)
		}
	}
	return nil
}

// checkScope is where an in-vocabulary, well-typed argument still gets denied.
// This is the layer that makes "the vocabulary is visible in the source you
// just read" an honest thing to tell an IT director.
func (a *Arg) checkScope(s string, scopes Scopes) *DenyError {
	switch a.Scope {
	case ScopeNone:
		return nil

	case ScopeDepot:
		// The literal prefix is everything before the first wildcard. A path
		// whose literal prefix does not already sit inside a scoped prefix is
		// denied, so //... is denied even for a P4 user who could read it: the
		// wildcard cannot climb above the scope.
		if !withinPrefixList(literalPrefix(s), scopes.DepotScope) {
			return deny(ReasonOutOfScopePath, "%s is outside depot_scope", a.Name)
		}
		return nil

	case ScopeAllowedBuildTys:
		if !containsExact(s, scopes.AllowedBuildTypes) {
			return deny(ReasonOutOfScopeBuildTy, "%s is not in allowed_build_types", a.Name)
		}
		return nil

	case ScopeRepoAllowlist:
		if !containsExact(s, scopes.RepoAllowlist) {
			return deny(ReasonOutOfScopeRepo, "%s is not in repo_allowlist", a.Name)
		}
		return nil
	}
	return deny(ReasonOutOfScopePath, "%s has an unknown scope", a.Name)
}

// literalPrefix returns the part of a depot path before the first Perforce
// wildcard. "//..." yields "//"; "//depot/game/..." yields "//depot/game/".
func literalPrefix(s string) string {
	if i := strings.IndexAny(s, "*."); i >= 0 {
		// "." alone is legal inside a filename; only "..." is the wildcard.
		if s[i] == '.' {
			if j := strings.Index(s, "..."); j >= 0 {
				return s[:j]
			}
			if k := strings.IndexByte(s, '*'); k >= 0 {
				return s[:k]
			}
			return s
		}
		return s[:i]
	}
	return s
}

// withinPrefixList reports whether s falls inside one of the configured depot
// scopes, matching only on a real path-segment boundary. A scope entry is
// expected to end with "/" (config.Validate normalizes every configured
// entry this way); an entry without a trailing slash is skipped rather than
// matched with a bare strings.HasPrefix, because that would let a scope of
// "//depot/game" wrongly admit "//depot/gamesecret/...": "gamesecret" also
// starts with the literal string "game". With the trailing slash required, s
// matches only when it is exactly the scope without its trailing slash (the
// scope root itself) or has the full "prefix/" as a genuine path prefix.
func withinPrefixList(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if !strings.HasSuffix(p, "/") {
			continue
		}
		if s == strings.TrimSuffix(p, "/") || strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func containsExact(s string, list []string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// BannedArgNames exposes the banned list for the vocabulary test.
func BannedArgNames() map[string]string { return bannedArgNames }

// ErrBannedArg is returned by Selfcheck.
var ErrBannedArg = errors.New("vocab: verb declares a banned argument name")

// Selfcheck asserts the structural invariants of the vocabulary. It runs in the
// test suite and again at process start, so a build that violates one of these
// refuses to run rather than shipping a quietly weaker allowlist.
func Selfcheck() error {
	for i := range Vocabulary {
		v := &Vocabulary[i]
		if v.Compiled && v.Mutating {
			return fmt.Errorf("vocab: %s is mutating and compiled; no X-class verb ships in v0", v.Name)
		}
		if v.Compiled && v.Class == ClassContent {
			return fmt.Errorf("vocab: %s is content class and compiled; content verbs are off in v0", v.Name)
		}
		for j := range v.Args {
			a := &v.Args[j]
			if why, banned := bannedArgNames[strings.ToLower(a.Name)]; banned {
				return fmt.Errorf("%w: %s.%s (%s)", ErrBannedArg, v.Name, a.Name, why)
			}
			switch a.Kind {
			case KindInt, KindBool, KindString, KindDepotPath:
			default:
				return fmt.Errorf("vocab: %s.%s has non-scalar kind %q", v.Name, a.Name, a.Kind)
			}
			if a.Kind == KindInt && a.Max <= 0 {
				return fmt.Errorf("vocab: %s.%s is an unbounded integer", v.Name, a.Name)
			}
			if a.Kind == KindString && a.MaxLen <= 0 {
				return fmt.Errorf("vocab: %s.%s is an unbounded string", v.Name, a.Name)
			}
		}
	}
	return nil
}
