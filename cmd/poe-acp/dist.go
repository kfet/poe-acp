// `poe-acp dist`: operator-facing management of the versioned binary
// layout (internal/install) that makes a binary swap reversible.
//
// This is the substrate's write side. Nothing here fetches anything: a
// binary is delivered by whatever is doing the delivering (today
// scripts/converge.sh over ssh, later `poe-acp reconcile`) and handed to
// `dist install`, which stages it under versions/ atomically. `dist
// activate` is the swap itself — a single atomic symlink repoint that
// the running supervisor picks up on its next fork, and the init system
// picks up on its next start, with no unit-file edit.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/kfet/poe-acp/internal/install"
)

const distUsage = `usage: poe-acp dist <command> [flags]

commands:
  status                 report the layout: current, last-good, pins, crashes
  install <file>         stage <file> as -version under versions/ (does not activate)
  activate <version>     repoint ` + "`current`" + ` at <version> (refuses a pinned version)
  unpin <version>        clear a crash-loop pin so <version> may be activated again

flags:
`

// runDist implements the `dist` subcommand. It returns the process exit
// code and writes everything it has to say to out/errOut, so it is
// driven directly in tests.
func runDist(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("dist", flag.ContinueOnError)
	fs.SetOutput(errOut)
	root := fs.String("root", "", "layout root (default: $POEACP_INSTALL_ROOT, else $XDG_STATE_HOME/poe-acp/dist)")
	scope := fs.String("scope", "", "crash-counter scope, normally the bot name")
	version := fs.String("version", "", "version to stage (install)")
	force := fs.Bool("force", false, "activate a version even if it is pinned")
	fs.Usage = func() {
		fmt.Fprint(errOut, distUsage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	l := install.New(install.Config{Root: *root, Scope: *scope})

	switch cmd := fs.Arg(0); cmd {
	case "status":
		return distStatus(l, out)
	case "install":
		return distInstall(l, fs.Arg(1), *version, out, errOut)
	case "activate":
		return distActivate(l, fs.Arg(1), *force, out, errOut)
	case "unpin":
		return distUnpin(l, fs.Arg(1), out, errOut)
	default:
		fmt.Fprintf(errOut, "dist: unknown command %q\n", cmd)
		fs.Usage()
		return 2
	}
}

// distStatus prints the layout state, including WHY rollback is
// unavailable on a host that still has a plain-file install.
func distStatus(l install.Layout, out io.Writer) int {
	fmt.Fprintf(out, "root:      %s\n", l.Root())
	if err := l.Managed(); err != nil {
		fmt.Fprintf(out, "layout:    UNMANAGED (%v)\n", err)
		fmt.Fprintf(out, "           rollback is unavailable; point the supervisor's ExecStart at %s\n", l.CurrentPath())
		return 0
	}
	cur, _ := l.CurrentVersion()
	fmt.Fprintf(out, "current:   %s\n", cur)
	if lg, err := l.LastGoodVersion(); err == nil {
		fmt.Fprintf(out, "last-good: %s\n", lg)
	} else {
		fmt.Fprintf(out, "last-good: (none yet — no worker has confirmed healthy)\n")
	}
	pins, err := l.Pinned()
	if err != nil {
		fmt.Fprintf(out, "pinned:    ERROR %v\n", err)
		return 1
	}
	versions := make([]string, 0, len(pins))
	for v := range pins {
		versions = append(versions, v)
	}
	sort.Strings(versions)
	for _, v := range versions {
		fmt.Fprintf(out, "pinned:    %s (%s, %s)\n", v, pins[v].Reason, pins[v].At.Format("2006-01-02T15:04:05Z"))
	}
	crashes, err := l.Crashes()
	if err != nil {
		fmt.Fprintf(out, "crashes:   ERROR %v\n", err)
		return 1
	}
	fmt.Fprintf(out, "crashes:   %d recorded\n", len(crashes))
	fmt.Fprintf(out, "log:       %s\n", l.LogPath())
	return 0
}

func distInstall(l install.Layout, path, version string, out, errOut io.Writer) int {
	if path == "" || version == "" {
		fmt.Fprintln(errOut, "dist install: need a file argument and -version")
		return 2
	}
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintln(errOut, "dist install:", err)
		return 1
	}
	defer func() { _ = f.Close() }()
	if err := l.Install(version, f); err != nil {
		fmt.Fprintln(errOut, "dist install:", err)
		return 1
	}
	fmt.Fprintf(out, "installed %s -> %s\n", version, l.VersionPath(version))
	return 0
}

func distActivate(l install.Layout, version string, force bool, out, errOut io.Writer) int {
	if version == "" {
		fmt.Fprintln(errOut, "dist activate: need a version argument")
		return 2
	}
	pinned, pin, err := l.IsPinned(version)
	if err != nil {
		fmt.Fprintln(errOut, "dist activate:", err)
		return 1
	}
	if pinned && !force {
		fmt.Fprintf(errOut, "dist activate: %s is pinned (%s); ship a newer build or pass -force\n", version, pin.Reason)
		return 1
	}
	if err := l.SwapCurrent(version); err != nil {
		fmt.Fprintln(errOut, "dist activate:", err)
		return 1
	}
	if err := l.Logf("activate version=%s force=%v", version, force); err != nil {
		fmt.Fprintln(errOut, "dist activate: log:", err)
	}
	fmt.Fprintf(out, "current -> %s\n", version)
	fmt.Fprintf(out, "reload the supervisor to swap workers onto it (systemctl --user reload poe-acp-<bot>)\n")
	return 0
}

func distUnpin(l install.Layout, version string, out, errOut io.Writer) int {
	if version == "" {
		fmt.Fprintln(errOut, "dist unpin: need a version argument")
		return 2
	}
	if err := l.Unpin(version); err != nil {
		fmt.Fprintln(errOut, "dist unpin:", err)
		return 1
	}
	fmt.Fprintf(out, "unpinned %s\n", version)
	return 0
}
