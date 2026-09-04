// Command butterstack-connector is the outbound-only daemon a studio runs
// inside its own network so ButterStack can reach a private Perforce or
// TeamCity without the studio opening a single inbound port.
//
// It opens exactly one outbound TLS connection to one hostname on 443 and never
// listens on anything. Everything it will do is in internal/vocab/vocab.go.
//
// Spike scope (issue #1575, group 1): teamcity.server.info, teamcity.build.get,
// p4.describe, p4.changes and the sys verbs. No mutating verb and no
// content-class verb is compiled into this build.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/ButterStack/butterstack-connector/internal/audit"
	"github.com/ButterStack/butterstack-connector/internal/config"
	"github.com/ButterStack/butterstack-connector/internal/protocol"
	"github.com/ButterStack/butterstack-connector/internal/session"
	"github.com/ButterStack/butterstack-connector/internal/vocab"
)

// Version is the connector build version, reported in hello and in sys.version.
// It is overridden at build time with -ldflags "-X main.Version=...".
var Version = "0.0.0-spike"

func main() {
	var (
		cfgPath   = flag.String("config", "/etc/butterstack/connector.yml", "path to connector.yml")
		showVocab = flag.Bool("print-vocabulary", false, "print the compiled command allowlist and exit")
		showVer   = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()

	if *showVer {
		fmt.Printf("butterstack-connector %s (protocol v%s)\n", Version, protocol.Version)
		return
	}
	if *showVocab {
		printVocabulary()
		return
	}

	if err := run(*cfgPath); err != nil {
		fmt.Fprintf(os.Stderr, "butterstack-connector: %v\n", err)
		os.Exit(1)
	}
}

func run(cfgPath string) error {
	// The vocabulary's structural invariants are checked before anything else,
	// so a build whose allowlist grew a banned argument refuses to start rather
	// than running with a quietly weaker guarantee.
	if err := vocab.Selfcheck(); err != nil {
		return err
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}

	log, err := audit.New(cfg.LogDir, os.Stderr)
	if err != nil {
		return err
	}
	defer log.Close()

	log.Event("startup", fmt.Sprintf("butterstack-connector %s protocol v%s", Version, protocol.Version))
	for _, line := range splitLines(cfg.Redacted()) {
		log.Event("config", line)
	}
	if os.Geteuid() == 0 {
		log.Event("warning", "running as root; the install doc asks for a dedicated non-root user")
	}

	runner, err := session.New(cfg, log, Version)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	err = runner.Run(ctx)
	log.Event("shutdown", "signal received; socket closed and audit log flushed")
	return err
}

func printVocabulary() {
	fmt.Printf("butterstack-connector %s, protocol v%s\n\n", Version, protocol.Version)
	for i := range vocab.Vocabulary {
		v := &vocab.Vocabulary[i]
		state := "RESERVED (denied)"
		if v.Compiled {
			state = "compiled"
		}
		fmt.Printf("%-26s class=%s %s\n", v.Name, v.Class, state)
		for j := range v.Args {
			a := &v.Args[j]
			req := ""
			if a.Required {
				req = " required"
			}
			scope := ""
			if a.Scope != "" {
				scope = fmt.Sprintf(" scope=%s", a.Scope)
			}
			fmt.Printf("    %-16s %s%s%s\n", a.Name, a.Kind, req, scope)
		}
	}
	fmt.Printf("\nNo verb accepts a host, port, URL, or shell string. There is no sys.exec.\n")
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
