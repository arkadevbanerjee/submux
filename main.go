// Command submux is a local relay that dispatches Claude Code subagent
// requests to different upstreams (and different credentials) based on the
// model id in each request body. See README.md for the mechanism.
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(64)
	}

	switch os.Args[1] {
	case "serve":
		cmdServe(os.Args[2:])
	case "routes":
		cmdRoutes(os.Args[2:])
	case "check":
		cmdCheck(os.Args[2:])
	case "models":
		cmdModels(os.Args[2:])
	case "status":
		cmdStatus(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "submux: unknown command %q\n", os.Args[1])
		usage()
		os.Exit(64)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  submux serve [--config PATH] [--listen ADDR] [--debug-headers]
  submux routes [--config PATH]
  submux check <model-id> [--config PATH]
  submux models [--config PATH]
  submux status [--config PATH] [--listen ADDR]`)
}

func resolveConfigPath(flagVal string) (string, error) {
	if flagVal != "" {
		return flagVal, nil
	}
	return defaultConfigPath()
}

func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	configPath := fs.String("config", "", "path to config.json (default ~/.config/submux/config.json)")
	listen := fs.String("listen", "", "listen address, overrides the config file's \"listen\"")
	debugHeaders := fs.Bool("debug-headers", false, "log header NAMES and a redacted Authorization summary per request")
	_ = fs.Parse(args)

	path, err := resolveConfigPath(*configPath)
	if err != nil {
		log.Fatalf("submux: %v", err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		log.Fatalf("submux: %v", err)
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	if err := resolveCredentials(cfg); err != nil {
		log.Fatalf("submux: %v", err)
	}

	srv := newServer(cfg, *debugHeaders)

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		log.Fatalf("submux: listen on %s: %v", cfg.Listen, err)
	}
	log.Printf("submux: listening on %s, %d route(s) from %s", cfg.Listen, len(cfg.Routes), path)
	for _, r := range cfg.Routes {
		log.Printf("submux: route %s", describeRoute(r))
	}

	httpSrv := &http.Server{Handler: srv}
	if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
		log.Fatalf("submux: serve: %v", err)
	}
}

func cmdRoutes(args []string) {
	fs := flag.NewFlagSet("routes", flag.ExitOnError)
	configPath := fs.String("config", "", "path to config.json")
	_ = fs.Parse(args)

	path, err := resolveConfigPath(*configPath)
	if err != nil {
		log.Fatalf("submux: %v", err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		log.Fatalf("submux: %v", err)
	}
	for _, r := range cfg.Routes {
		fmt.Println(describeRoute(r))
	}
}

func cmdCheck(args []string) {
	// Parsed by hand, not flag.FlagSet: the documented usage is
	// "check <model-id> [--config PATH]", i.e. the flag may come AFTER the
	// positional model id, and flag.FlagSet stops parsing at the first
	// non-flag argument so it can't express that ordering.
	var modelID, configPath string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--config":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "usage: submux check <model-id> [--config PATH]")
				os.Exit(64)
			}
			configPath = args[i+1]
			i++
		default:
			if modelID != "" {
				fmt.Fprintln(os.Stderr, "usage: submux check <model-id> [--config PATH]")
				os.Exit(64)
			}
			modelID = args[i]
		}
	}
	if modelID == "" {
		fmt.Fprintln(os.Stderr, "usage: submux check <model-id> [--config PATH]")
		os.Exit(64)
	}

	path, err := resolveConfigPath(configPath)
	if err != nil {
		log.Fatalf("submux: %v", err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		log.Fatalf("submux: %v", err)
	}

	rt, matched := matchRoute(cfg.Routes, modelID)
	if !matched {
		fmt.Fprintf(os.Stderr, "submux check %q: no route matched\n", modelID)
		os.Exit(1)
	}

	fmt.Printf("model=%q\n", modelID)
	fmt.Printf("  route        %s\n", describeRoute(rt))

	res := resolveSubscription(cfg, rt, modelID)
	switch res.Status {
	case "fixed", "resolved":
		fmt.Printf("  subscription %s\n", res.Name)
		fmt.Printf("  evidence     %s\n", res.Evidence)
	case "not_served":
		fmt.Printf("  subscription NOT SERVED by %s\n", rt.upstream)
		if len(res.Suggestions) > 0 {
			fmt.Printf("  nearest      %s\n", strings.Join(res.Suggestions, ", "))
		}
		os.Exit(1)
	case "unknown":
		fmt.Printf("  subscription UNKNOWN: %v\n", res.Err)
		os.Exit(2)
	}
}

// cmdModels implements `submux models` (§2.5): fetch every non-fixed-label
// route's /v1/models once, group ids by resolved subscription, print
// "SUBSCRIPTION (n ids): id, id, id ..." sorted by count desc. A route that
// carries a fixed subscription label is skipped -- it has no catalogue to
// list (the claude-* passthrough route, for example). A route whose
// credential or upstream fails is reported to stderr and skipped, so one
// broken aggregator does not blank the whole command.
func cmdModels(args []string) {
	fs := flag.NewFlagSet("models", flag.ExitOnError)
	configPath := fs.String("config", "", "path to config.json")
	_ = fs.Parse(args)

	path, err := resolveConfigPath(*configPath)
	if err != nil {
		log.Fatalf("submux: %v", err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		log.Fatalf("submux: %v", err)
	}

	buckets := map[string][]string{}
	for _, rt := range cfg.Routes {
		if rt.subscription != "" {
			continue
		}
		if err := resolveRouteCredential(&rt); err != nil {
			fmt.Fprintf(os.Stderr, "submux models: route match=%q: %v\n", rt.match, err)
			continue
		}
		models, err := fetchUpstreamModels(rt)
		if err != nil {
			fmt.Fprintf(os.Stderr, "submux models: route match=%q: %v\n", rt.match, err)
			continue
		}
		for _, m := range models {
			name, _ := subscriptionNameForOwnedBy(cfg, m.ID, m.OwnedBy)
			buckets[name] = append(buckets[name], m.ID)
		}
	}

	type row struct {
		name string
		ids  []string
	}
	rows := make([]row, 0, len(buckets))
	for name, ids := range buckets {
		sort.Strings(ids)
		rows = append(rows, row{name, ids})
	}
	sort.Slice(rows, func(i, j int) bool {
		if len(rows[i].ids) != len(rows[j].ids) {
			return len(rows[i].ids) > len(rows[j].ids)
		}
		return rows[i].name < rows[j].name
	})
	for _, r := range rows {
		fmt.Printf("%s (%d ids): %s\n", r.name, len(r.ids), strings.Join(r.ids, ", "))
	}
}

// cmdStatus queries a running `submux serve` process's admin endpoint and
// prints the currently-cooling model ids and the last 20 fallback events
// (§9.3). It never reads process memory directly -- fallback state is
// in-memory-only inside the serve process, so this is an HTTP call to it.
func cmdStatus(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	configPath := fs.String("config", "", "path to config.json (used to find the listen address)")
	listen := fs.String("listen", "", "address to query, overrides the config file's \"listen\"")
	_ = fs.Parse(args)

	addr := *listen
	if addr == "" {
		path, err := resolveConfigPath(*configPath)
		if err != nil {
			log.Fatalf("submux: %v", err)
		}
		cfg, err := loadConfig(path)
		if err != nil {
			log.Fatalf("submux: %v", err)
		}
		addr = cfg.Listen
	}

	st, err := fetchStatus(addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "submux: status: %v\n", err)
		os.Exit(1)
	}

	if len(st.Cooling) == 0 {
		fmt.Println("cooling: none")
	} else {
		fmt.Println("cooling:")
		for _, c := range st.Cooling {
			fmt.Printf("  %s until %s\n", c.Model, c.Until)
		}
	}

	if len(st.RecentFallbacks) == 0 {
		fmt.Println("recent fallbacks: none")
	} else {
		fmt.Println("recent fallbacks (most recent last):")
		for _, f := range st.RecentFallbacks {
			fmt.Printf("  %s  %s -> %v (%s)\n", f.At, f.Requested, f.Attempted, f.Outcome)
		}
	}
}
