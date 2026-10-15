package cli

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/OlivMartins/vigie/internal/config"
	"github.com/OlivMartins/vigie/internal/pins"
)

const pinsUsage = `Usage:
  vigie pins list [SERVER]                       show pinned tools and their status
  vigie pins diff SERVER TOOL                    show what changed in a tool definition
  vigie pins approve SERVER TOOL --fingerprint F  approve the definition you reviewed
  vigie pins approve SERVER [TOOL...] [--all]    approve pending definitions
  vigie pins forget SERVER TOOL...               drop tools: they count as new next time

Every subcommand accepts --config FILE.
`

func pinsCommand(args []string, s Streams) int {
	if len(args) == 0 {
		fmt.Fprint(s.Err, pinsUsage)
		return exitUsage
	}
	switch args[0] {
	case "list":
		return pinsList(args[1:], s)
	case "diff":
		return pinsDiff(args[1:], s)
	case "approve":
		return pinsApprove(args[1:], s)
	case "forget":
		return pinsForget(args[1:], s)
	case "-h", "-help", "--help", "help":
		fmt.Fprint(s.Out, pinsUsage)
		return exitOK
	default:
		fmt.Fprintf(s.Err, "vigie: unknown pins subcommand %q\n\n%s", args[0], pinsUsage)
		return exitUsage
	}
}

// pinsFlags parses the common --config flag and returns the positional
// arguments and the pins directory.
func pinsFlags(name, synopsis string, args []string, s Streams, extra func(*flag.FlagSet)) (positional []string, dir string, fs *flag.FlagSet, code int) {
	fs = newFlagSet("pins "+name, synopsis, s)
	configPath := fs.String("config", "", "configuration file (default: $"+config.EnvConfig+", then the user configuration directory)")
	if extra != nil {
		extra(fs)
	}
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		if isHelp(err) {
			return nil, "", fs, exitOK
		}
		return nil, "", fs, exitUsage
	}
	path, err := config.Resolve(*configPath)
	if err != nil {
		return nil, "", fs, fail(s, exitUsage, err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, "", fs, fail(s, exitUsage, err)
	}
	return positional, cfg.PinsDir, fs, -1
}

func pinsList(args []string, s Streams) int {
	positional, dir, fs, code := pinsFlags("list", "vigie pins list [SERVER]", args, s, nil)
	if code >= 0 {
		return code
	}
	if len(positional) > 1 {
		return usageError(s, fs, "too many arguments")
	}
	servers := positional
	if len(servers) == 0 {
		var err error
		if servers, err = pinnedServers(dir); err != nil {
			return fail(s, exitError, err)
		}
		if len(servers) == 0 {
			fmt.Fprintf(s.Out, "No pin file in %s yet: tools are pinned the first time Vigie relays a tools/list.\n", dir)
			return exitOK
		}
	}
	tw := tabwriter.NewWriter(s.Out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SERVER\tTOOL\tSTATUS\tAPPROVED\tCHANGE SEEN")
	for _, server := range servers {
		store, err := pins.Open(dir, server)
		if err != nil {
			return fail(s, exitUsage, err)
		}
		f, err := store.Load()
		if err != nil {
			return fail(s, exitError, err)
		}
		for _, name := range f.Names() {
			e := f.Tools[name]
			approved, seen := "-", "-"
			if e.Approved != nil && e.Approved.ApprovedAt != nil {
				approved = day(*e.Approved.ApprovedAt) + " (" + e.Approved.ApprovedBy + ")"
			}
			if e.Pending != nil {
				seen = e.Pending.SeenAt.UTC().Format("2006-01-02 15:04 UTC")
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", server, name, e.Status(), approved, seen)
		}
	}
	if err := tw.Flush(); err != nil {
		return fail(s, exitError, err)
	}
	return exitOK
}

func pinsDiff(args []string, s Streams) int {
	positional, dir, fs, code := pinsFlags("diff", "vigie pins diff SERVER TOOL", args, s, nil)
	if code >= 0 {
		return code
	}
	if len(positional) != 2 {
		return usageError(s, fs, "expected a server and a tool")
	}
	server, tool := positional[0], positional[1]
	store, err := pins.Open(dir, server)
	if err != nil {
		return fail(s, exitUsage, err)
	}
	f, err := store.Load()
	if err != nil {
		return fail(s, exitError, err)
	}
	e, ok := f.Tools[tool]
	if !ok {
		return fail(s, exitError, fmt.Errorf("tool %q is not in %s", tool, store.Path()))
	}
	red, green, bold := colors(s.Out)
	switch e.Status() {
	case pins.StatusApproved:
		fmt.Fprintf(s.Out, "Tool %s of server %s: approved on %s (%s), nothing waiting for review.\n",
			tool, server, day(*e.Approved.ApprovedAt), e.Approved.ApprovedBy)
		return exitOK
	case pins.StatusPending:
		fmt.Fprintln(s.Out, bold(fmt.Sprintf("New tool %s of server %s, first seen %s", tool, server, e.Pending.SeenAt.UTC().Format(time.RFC3339))))
		fmt.Fprintf(s.Out, "%s %s\n\n", green("+++"), e.Pending.Fingerprint)
		for _, line := range strings.Split(pins.Render(e.Pending.Definition), "\n") {
			fmt.Fprintln(s.Out, green("+ "+line))
		}
	case pins.StatusChanged:
		fmt.Fprintln(s.Out, bold(fmt.Sprintf("Tool %s of server %s changed", tool, server)))
		fmt.Fprintf(s.Out, "%s approved on %s (%s)  %s\n", red("---"), day(*e.Approved.ApprovedAt), e.Approved.ApprovedBy, short(e.Approved.Fingerprint))
		fmt.Fprintf(s.Out, "%s seen on %s  %s\n\n", green("+++"), e.Pending.SeenAt.UTC().Format(time.RFC3339), short(e.Pending.Fingerprint))
		for _, line := range strings.Split(strings.TrimRight(pins.Diff(e.Approved.Definition, e.Pending.Definition), "\n"), "\n") {
			switch {
			case strings.HasPrefix(line, "-"):
				line = red(line)
			case strings.HasPrefix(line, "+"):
				line = green(line)
			}
			fmt.Fprintln(s.Out, line)
		}
	}
	// The fingerprint ties the approval to this review: if the server sends
	// yet another definition in the meantime, the approval fails.
	fmt.Fprintf(s.Out, "\nApprove it with: vigie pins approve %s %s --fingerprint %s\n", server, tool, short(e.Pending.Fingerprint))
	return exitOK
}

// short abbreviates a fingerprint to its first 12 hexadecimal characters,
// enough to identify a reviewed definition.
func short(fingerprint string) string {
	if len(fingerprint) > len("sha256:")+12 {
		return fingerprint[:len("sha256:")+12]
	}
	return fingerprint
}

func pinsApprove(args []string, s Streams) int {
	var all bool
	var fingerprint string
	positional, dir, fs, code := pinsFlags("approve", "vigie pins approve SERVER TOOL --fingerprint F | SERVER [TOOL...] [--all]", args, s, func(fs *flag.FlagSet) {
		fs.BoolVar(&all, "all", false, "approve every pending definition of the server")
		fs.StringVar(&fingerprint, "fingerprint", "", "approve only if the pending definition still has this fingerprint, as shown by vigie pins diff")
	})
	if code >= 0 {
		return code
	}
	if len(positional) == 0 {
		return usageError(s, fs, "expected a server")
	}
	server, tools := positional[0], positional[1:]
	if len(tools) == 0 && !all {
		return usageError(s, fs, "name the tools to approve, or pass --all")
	}
	if len(tools) > 0 && all {
		return usageError(s, fs, "--all approves every pending tool: do not name tools with it")
	}
	if fingerprint != "" && len(tools) != 1 {
		return usageError(s, fs, "--fingerprint goes with exactly one tool")
	}
	store, err := pins.Open(dir, server)
	if err != nil {
		return fail(s, exitUsage, err)
	}
	if fingerprint != "" {
		if err := store.ApproveFingerprint(tools[0], fingerprint); err != nil {
			return fail(s, exitError, err)
		}
		fmt.Fprintf(s.Out, "Approved %s (server %s), definition %s.\n", tools[0], server, fingerprint)
		return exitOK
	}
	approved, err := store.Approve(tools...)
	if err != nil {
		return fail(s, exitError, err)
	}
	if len(approved) == 0 {
		fmt.Fprintf(s.Out, "Nothing waiting for approval for server %s.\n", server)
		return exitOK
	}
	for _, t := range approved {
		fmt.Fprintf(s.Out, "Approved %s (server %s).\n", t, server)
	}
	return exitOK
}

func pinsForget(args []string, s Streams) int {
	positional, dir, fs, code := pinsFlags("forget", "vigie pins forget SERVER TOOL...", args, s, nil)
	if code >= 0 {
		return code
	}
	if len(positional) < 2 {
		return usageError(s, fs, "expected a server and at least one tool")
	}
	store, err := pins.Open(dir, positional[0])
	if err != nil {
		return fail(s, exitUsage, err)
	}
	forgotten, err := store.Forget(positional[1:]...)
	if err != nil {
		return fail(s, exitError, err)
	}
	for _, t := range forgotten {
		fmt.Fprintf(s.Out, "Forgot %s (server %s): it will count as a new tool next time.\n", t, positional[0])
	}
	return exitOK
}

// pinnedServers lists the servers that have a pin file in dir.
func pinnedServers(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var servers []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".json" || strings.HasPrefix(name, ".") {
			continue
		}
		if server := strings.TrimSuffix(name, ".json"); pins.ValidServerName(server) {
			servers = append(servers, server)
		}
	}
	sort.Strings(servers)
	return servers, nil
}

func day(t time.Time) string { return t.UTC().Format("2006-01-02") }
