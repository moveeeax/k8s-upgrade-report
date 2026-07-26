// Command k8s-upgrade-report scans Kubernetes manifests for APIs that a
// version upgrade removes, generates the mechanical patches, and prints a
// readiness report with sequencing and a go/no-go.
//
// It reads files. It never connects to a cluster and never applies anything.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/moveeeax/k8s-upgrade-report/internal/patch"
	"github.com/moveeeax/k8s-upgrade-report/internal/plan"
	"github.com/moveeeax/k8s-upgrade-report/internal/report"
	"github.com/moveeeax/k8s-upgrade-report/internal/rules"
	"github.com/moveeeax/k8s-upgrade-report/internal/scan"
)

// version is overridden at build time with -ldflags "-X main.version=v1.2.3".
var version = "dev"

const usage = `k8s-upgrade-report - Kubernetes upgrade readiness for manifests you already have

Usage:
  k8s-upgrade-report scan   --manifests DIR --target 1.29 [flags]
  k8s-upgrade-report patch  --manifests DIR --target 1.29 [flags]
  k8s-upgrade-report rules  [--provider eks]
  k8s-upgrade-report version

Commands:
  scan     Report what breaks, in what order, and whether it is a go.
  patch    Print a git-applyable diff for the mechanical rewrites only.
  rules    Print the checked-in removal dataset and addon floors.

Run "k8s-upgrade-report <command> -h" for the flags of a command.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}
	var err error
	code := 0
	switch os.Args[1] {
	case "scan":
		code, err = runScan(os.Args[2:])
	case "patch":
		err = runPatch(os.Args[2:])
	case "rules":
		err = runRules(os.Args[2:])
	case "version":
		fmt.Printf("k8s-upgrade-report %s\n", version)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "k8s-upgrade-report: %v\n", err)
		os.Exit(1)
	}
	os.Exit(code)
}

type commonFlags struct {
	manifests string
	target    string
	current   string
	provider  string
}

func (c *commonFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&c.manifests, "manifests", "", "directory (or single file) of Kubernetes manifests to scan")
	fs.StringVar(&c.target, "target", "", "Kubernetes minor version being upgraded to, e.g. 1.29")
	fs.StringVar(&c.current, "current", "", "Kubernetes minor version the cluster runs today (default: one minor below --target)")
	fs.StringVar(&c.provider, "provider", "", "managed offering: eks, aks or gke (adds addon floors and sequencing)")
}

func (c *commonFlags) resolve(set *rules.Set) (scan.Options, plan.Options, error) {
	if c.manifests == "" {
		return scan.Options{}, plan.Options{}, fmt.Errorf("--manifests is required")
	}
	if c.target == "" {
		return scan.Options{}, plan.Options{}, fmt.Errorf("--target is required, e.g. --target 1.29")
	}
	target, err := rules.ParseMinor(c.target)
	if err != nil {
		return scan.Options{}, plan.Options{}, fmt.Errorf("--target: %w", err)
	}
	current := rules.Minor{Major: target.Major, Minor: target.Minor - 1}
	if c.current != "" {
		if current, err = rules.ParseMinor(c.current); err != nil {
			return scan.Options{}, plan.Options{}, fmt.Errorf("--current: %w", err)
		}
	}
	if rules.MinorLess(target, current) {
		return scan.Options{}, plan.Options{}, fmt.Errorf("--target %s is older than --current %s", target, current)
	}
	if c.provider != "" {
		if _, ok := set.Provider(c.provider); !ok {
			return scan.Options{}, plan.Options{}, fmt.Errorf("--provider %q is not supported; known providers: %s",
				c.provider, strings.Join(set.ProviderNames(), ", "))
		}
	}
	return scan.Options{Current: current, Target: target},
		plan.Options{Current: current, Target: target, Provider: c.provider}, nil
}

func runScan(args []string) (int, error) {
	fs := flag.NewFlagSet("scan", flag.ExitOnError)
	var c commonFlags
	c.register(fs)
	format := fs.String("format", "text", "stdout format: text or json")
	htmlOut := fs.String("html", "", "also write a self-contained HTML report to this path")
	patchOut := fs.String("patch", "", "also write the generated diff to this path")
	failOn := fs.String("fail-on", "none", "exit 2 when findings of this severity exist: none, warning or blocker")
	if err := fs.Parse(args); err != nil {
		return 1, err
	}

	set, err := rules.Load()
	if err != nil {
		return 1, err
	}
	scanOpts, planOpts, err := c.resolve(set)
	if err != nil {
		return 1, err
	}

	files, skipped, err := scan.LoadDir(c.manifests)
	if err != nil {
		return 1, err
	}
	res := scan.Run(set, files, skipped, scanOpts)
	p := plan.Build(set, res, planOpts)

	patched := 0
	if *patchOut != "" {
		pr, err := patch.Generate(set, files, res.Findings)
		if err != nil {
			return 1, err
		}
		patched = len(pr.Patched)
		if err := os.WriteFile(*patchOut, []byte(pr.Diff), 0o644); err != nil {
			return 1, err
		}
		fmt.Fprintf(os.Stderr, "wrote %s (%d object(s) rewritten)\n", *patchOut, patched)
	}

	rep := report.Build(version, filepath.ToSlash(c.manifests), time.Now(), res, p, patched)

	switch *format {
	case "json":
		if err := rep.WriteJSON(os.Stdout); err != nil {
			return 1, err
		}
	case "text":
		if err := rep.WriteText(os.Stdout); err != nil {
			return 1, err
		}
	default:
		return 1, fmt.Errorf("--format must be text or json")
	}

	if *htmlOut != "" {
		f, err := os.Create(*htmlOut)
		if err != nil {
			return 1, err
		}
		defer f.Close()
		if err := rep.WriteHTML(f); err != nil {
			return 1, err
		}
		fmt.Fprintf(os.Stderr, "wrote %s\n", *htmlOut)
	}

	switch *failOn {
	case "blocker":
		if rep.Summary.Blockers > 0 {
			return 2, nil
		}
	case "warning":
		if rep.Summary.Blockers+rep.Summary.Warnings > 0 {
			return 2, nil
		}
	case "none", "":
	default:
		return 1, fmt.Errorf("--fail-on must be none, warning or blocker")
	}
	return 0, nil
}

func runPatch(args []string) error {
	fs := flag.NewFlagSet("patch", flag.ExitOnError)
	var c commonFlags
	c.register(fs)
	out := fs.String("out", "-", "write the diff here; - means stdout")
	if err := fs.Parse(args); err != nil {
		return err
	}

	set, err := rules.Load()
	if err != nil {
		return err
	}
	scanOpts, _, err := c.resolve(set)
	if err != nil {
		return err
	}
	files, skipped, err := scan.LoadDir(c.manifests)
	if err != nil {
		return err
	}
	res := scan.Run(set, files, skipped, scanOpts)
	pr, err := patch.Generate(set, files, res.Findings)
	if err != nil {
		return err
	}

	if *out == "-" {
		fmt.Print(pr.Diff)
	} else if err := os.WriteFile(*out, []byte(pr.Diff), 0o644); err != nil {
		return err
	}
	for _, s := range pr.Skipped {
		fmt.Fprintf(os.Stderr, "not patched: %s\n", s)
	}
	fmt.Fprintf(os.Stderr, "%d object(s) rewritten, %d left for a human\n", len(pr.Patched), len(pr.Skipped))
	return nil
}

func runRules(args []string) error {
	fs := flag.NewFlagSet("rules", flag.ExitOnError)
	provider := fs.String("provider", "", "print addon floors and sequencing for eks, aks or gke")
	if err := fs.Parse(args); err != nil {
		return err
	}
	set, err := rules.Load()
	if err != nil {
		return err
	}
	if *provider != "" {
		p, ok := set.Provider(*provider)
		if !ok {
			return fmt.Errorf("unknown provider %q; known providers: %s", *provider, strings.Join(set.ProviderNames(), ", "))
		}
		fmt.Printf("%s sequencing\n", p.Name)
		for i, s := range p.Sequencing {
			fmt.Printf("  %d. %s\n", i+1, s)
		}
		return nil
	}
	fmt.Printf("%-10s %-48s %-34s %s\n", "REMOVED", "API", "REPLACEMENT", "PATCHABLE")
	for _, r := range set.Rules() {
		to := r.To
		if to == "" {
			to = "(none)"
		}
		auto := "manual"
		if r.Autofixable() {
			auto = string(r.Transform)
		}
		fmt.Printf("%-10s %-48s %-34s %s\n", r.RemovedIn, r.From+" "+r.Kind, to, auto)
	}
	return nil
}
