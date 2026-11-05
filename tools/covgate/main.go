// Command covgate reads one or more Go coverage profiles (as produced by
// `go test -coverprofile`) and turns them into an actionable report plus a
// pass/fail gate.
//
// Why this exists: `go tool cover -func=cover.out | tail -1` only prints the
// grand total. That number hides the thing reviewers actually care about —
// which packages are dragging it down, and whether a change silently dropped
// one of them. covgate groups the profile by package, ranks the worst ones,
// and fails the build when the total or a named package crosses a floor.
//
// Usage:
//
//	go run ./tools/covgate -profile cover.out -min 73 -pkg-min server=70
//	go run ./tools/covgate -profile cover.out -summary "$GITHUB_STEP_SUMMARY"
//
// Exit codes: 0 = gate passed, 1 = gate failed, 2 = bad input.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
)

// block is one coverage region as it appears in a profile line:
//
//	<file>:<startLine>.<startCol>,<endLine>.<endCol> <numStmt> <count>
type block struct {
	stmts int
	count int
}

// profile holds every block keyed by file:range. The same block can show up
// more than once when a package is instrumented by several test packages via
// -coverpkg, so we keep the highest count instead of double counting.
type profile struct {
	blocks map[string]block
	order  []string // insertion order, keeps output deterministic
}

func newProfile() *profile { return &profile{blocks: map[string]block{}} }

func (p *profile) add(key string, stmts, count int) {
	if old, ok := p.blocks[key]; ok {
		if count > old.count {
			old.count = count
			p.blocks[key] = old
		}
		return
	}
	p.blocks[key] = block{stmts: stmts, count: count}
	p.order = append(p.order, key)
}

// parseProfile reads profile data. A leading "mode:" line is required by the
// format but carries no per-block information, so it is validated and skipped.
func parseProfile(r io.Reader) (*profile, error) {
	p := newProfile()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	lineNo := 0
	sawMode := false
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "mode:") {
			if sawMode {
				return nil, fmt.Errorf("line %d: duplicate mode line", lineNo)
			}
			sawMode = true
			continue
		}
		if !sawMode {
			return nil, fmt.Errorf("line %d: missing mode line", lineNo)
		}

		// Split off the location, then the two trailing numbers.
		loc, rest, ok := strings.Cut(line, " ")
		if !ok {
			return nil, fmt.Errorf("line %d: want \"<file>:<range> <stmts> <count>\", got %q", lineNo, line)
		}
		fields := strings.Fields(rest)
		if len(fields) != 2 {
			return nil, fmt.Errorf("line %d: want 2 trailing numbers, got %d in %q", lineNo, len(fields), line)
		}
		stmts, err := strconv.Atoi(fields[0])
		if err != nil || stmts < 0 {
			return nil, fmt.Errorf("line %d: bad statement count %q", lineNo, fields[0])
		}
		count, err := strconv.Atoi(fields[1])
		if err != nil || count < 0 {
			return nil, fmt.Errorf("line %d: bad hit count %q", lineNo, fields[1])
		}
		// Guard against a file path containing no range separator: the value
		// after the last ':' must look like "12.3,45.6".
		if !strings.Contains(loc[strings.LastIndex(loc, ":")+1:], ",") {
			return nil, fmt.Errorf("line %d: bad block location %q", lineNo, loc)
		}
		p.add(loc, stmts, count)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if !sawMode {
		return nil, errors.New("empty profile: no mode line")
	}
	return p, nil
}

// pkgStat is the statement-weighted coverage of one package.
type pkgStat struct {
	name    string
	covered int
	total   int
}

func (s pkgStat) percent() float64 {
	if s.total == 0 {
		return 100
	}
	return float64(s.covered) / float64(s.total) * 100
}

// byPackage groups blocks by directory of the file path. Import-path prefixes
// (github.com/owner/repo/server/x.go) and plain relative paths both work
// because only the directory component matters.
func (p *profile) byPackage() map[string]*pkgStat {
	out := map[string]*pkgStat{}
	for _, key := range p.order {
		b := p.blocks[key]
		file := key[:strings.LastIndex(key, ":")]
		dir := path.Dir(file)
		if dir == "." || dir == "/" {
			dir = file
		}
		st, ok := out[dir]
		if !ok {
			st = &pkgStat{name: dir}
			out[dir] = st
		}
		st.total += b.stmts
		if b.count > 0 {
			st.covered += b.stmts
		}
	}
	return out
}

func (p *profile) total() pkgStat {
	t := pkgStat{name: "TOTAL"}
	for _, b := range p.blocks {
		t.total += b.stmts
		if b.count > 0 {
			t.covered += b.stmts
		}
	}
	return t
}

// pkgFloor is a "-pkg-min name=pct" requirement.
type pkgFloor struct {
	name string
	min  float64
}

// stringList collects a repeatable string flag.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	if strings.TrimSpace(v) == "" {
		return errors.New("empty value")
	}
	*s = append(*s, v)
	return nil
}

// floorList collects repeatable "name=pct" flags.
type floorList []pkgFloor

func (f *floorList) String() string {
	parts := make([]string, 0, len(*f))
	for _, it := range *f {
		parts = append(parts, fmt.Sprintf("%s=%g", it.name, it.min))
	}
	return strings.Join(parts, ",")
}

func (f *floorList) Set(v string) error {
	name, num, ok := strings.Cut(v, "=")
	if !ok {
		return fmt.Errorf("want name=pct, got %q", v)
	}
	name = strings.TrimSpace(name)
	pct, err := strconv.ParseFloat(strings.TrimSpace(num), 64)
	if err != nil {
		return fmt.Errorf("bad percentage in %q: %v", v, err)
	}
	if name == "" {
		return fmt.Errorf("empty package name in %q", v)
	}
	*f = append(*f, pkgFloor{name: name, min: pct})
	return nil
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		var gate *gateError
		if errors.As(err, &gate) {
			fmt.Fprintf(os.Stderr, "covgate: %v\n", gate)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "covgate: %v\n", err)
		os.Exit(2)
	}
}

type gateError struct{ msg string }

func (e *gateError) Error() string { return e.msg }

func run(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("covgate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var profiles stringList
	fs.Var(&profiles, "profile", "coverage profile path (repeatable)")
	minTotal := fs.Float64("min", 0, "minimum total statement coverage in percent (0 = no gate)")
	top := fs.Int("top", 10, "how many of the worst packages to list (0 = all)")
	summary := fs.String("summary", "", "append a markdown summary to this file (e.g. $GITHUB_STEP_SUMMARY)")
	quiet := fs.Bool("quiet", false, "only print the total line")

	var floors floorList
	fs.Var(&floors, "pkg-min", "per-package floor as name=pct (repeatable)")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(profiles) == 0 {
		return errors.New("-profile is required")
	}
	if *minTotal < 0 || *minTotal > 100 {
		return fmt.Errorf("-min out of range: %g", *minTotal)
	}

	merged := newProfile()
	for _, path := range profiles {
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open profile: %w", err)
		}
		part, perr := parseProfile(f)
		cerr := f.Close()
		if perr != nil {
			return fmt.Errorf("%s: %w", path, perr)
		}
		if cerr != nil {
			return fmt.Errorf("close %s: %w", path, cerr)
		}
		for _, key := range part.order {
			b := part.blocks[key]
			merged.add(key, b.stmts, b.count)
		}
	}

	full := merged.byPackage()
	pkgs := shortenPackages(full)
	stats := make([]pkgStat, 0, len(pkgs))
	for _, st := range pkgs {
		stats = append(stats, *st)
	}
	sort.Slice(stats, func(i, j int) bool {
		if stats[i].percent() != stats[j].percent() {
			return stats[i].percent() < stats[j].percent()
		}
		return stats[i].name < stats[j].name
	})

	total := merged.total()
	fmt.Fprintf(stdout, "总语句覆盖 %.1f%% (%d/%d)\n", total.percent(), total.covered, total.total)

	shown := stats
	if *top > 0 && len(shown) > *top {
		shown = shown[:*top]
	}
	if !*quiet {
		if len(shown) > 0 {
			fmt.Fprintf(stdout, "最低的 %d 个包：\n", len(shown))
		}
		width := 0
		for _, st := range shown {
			if n := len(st.name); n > width {
				width = n
			}
		}
		for _, st := range shown {
			fmt.Fprintf(stdout, "  %-*s  %5.1f%%  %6d/%-6d %s\n",
				width, st.name, st.percent(), st.covered, st.total, bar(st.percent()))
		}
	}

	var failures []string
	if *minTotal > 0 && round1(total.percent()) < round1(*minTotal) {
		failures = append(failures, fmt.Sprintf("总覆盖 %.1f%% < 阈值 %.1f%%", total.percent(), *minTotal))
	}
	for _, fl := range floors {
		st, ok := lookupPackage(pkgs, full, fl.name)
		if !ok {
			// A floor for a package with no profile data means the tests for it
			// did not run — that must fail loudly instead of silently passing.
			failures = append(failures, fmt.Sprintf("包 %s 无覆盖数据（期望 ≥ %.1f%%）", fl.name, fl.min))
			continue
		}
		if round1(st.percent()) < round1(fl.min) {
			failures = append(failures, fmt.Sprintf("包 %s 覆盖 %.1f%% < 阈值 %.1f%%", fl.name, st.percent(), fl.min))
		}
	}

	if *summary != "" {
		if err := writeSummary(*summary, total, shown, *minTotal, failures); err != nil {
			return err
		}
	}

	if len(failures) > 0 {
		for _, f := range failures {
			fmt.Fprintf(stdout, "FAIL %s\n", f)
		}
		return &gateError{msg: strings.Join(failures, "; ")}
	}
	if *minTotal > 0 || len(floors) > 0 {
		fmt.Fprintln(stdout, "覆盖率门禁通过")
	}
	return nil
}

// shortenPackages renames packages relative to their common parent directory.
// A `go test ./...` profile stores full import paths, so every key starts with
// the module path (github.com/Autumn-27/artex/...). Trimming the shared part
// keeps the report and the -pkg-min flags readable ("server" instead of the
// whole import path) without hardcoding the module name.
func shortenPackages(full map[string]*pkgStat) map[string]*pkgStat {
	out := make(map[string]*pkgStat, len(full))
	if len(full) == 0 {
		return out
	}
	names := make([]string, 0, len(full))
	for name := range full {
		names = append(names, name)
	}
	sort.Strings(names)

	prefix := strings.Split(names[0], "/")
	for _, name := range names[1:] {
		parts := strings.Split(name, "/")
		n := 0
		for n < len(prefix) && n < len(parts) && prefix[n] == parts[n] {
			n++
		}
		prefix = prefix[:n]
	}
	// Never strip the last element: a single-package profile would otherwise
	// collapse to an empty name.
	if len(prefix) >= len(strings.Split(names[0], "/")) {
		prefix = prefix[:len(prefix)-1]
	}

	for _, name := range names {
		parts := strings.Split(name, "/")
		short := strings.Join(parts[len(prefix):], "/")
		if short == "" {
			short = parts[len(parts)-1]
		}
		st := *full[name]
		st.name = short
		out[short] = &st
	}
	return out
}

// lookupPackage resolves a -pkg-min name against the shortened keys first, then
// the raw profile keys, then a unique path suffix. The suffix pass keeps names
// like "cmd/artex" working even when the module prefix was not trimmed.
func lookupPackage(short, full map[string]*pkgStat, name string) (*pkgStat, bool) {
	if st, ok := short[name]; ok {
		return st, true
	}
	if st, ok := full[name]; ok {
		return st, true
	}
	var found *pkgStat
	for key, st := range full {
		if strings.HasSuffix(key, "/"+name) {
			if found != nil {
				return nil, false // ambiguous: force the caller to be precise
			}
			found = st
		}
	}
	return found, found != nil
}

// round1 compares percentages at one decimal so that 72.96 and 73.0 are not
// judged as different by floating point noise.
func round1(v float64) float64 { return math.Round(v*10) / 10 }

// bar renders a 10-cell ASCII bar; ASCII keeps Windows consoles readable.
func bar(pct float64) string {
	cells := int(math.Round(pct / 10))
	if cells < 0 {
		cells = 0
	}
	if cells > 10 {
		cells = 10
	}
	return "[" + strings.Repeat("#", cells) + strings.Repeat(".", 10-cells) + "]"
}

func writeSummary(file string, total pkgStat, stats []pkgStat, minTotal float64, failures []string) error {
	var b strings.Builder
	b.WriteString("### 覆盖率\n\n")
	fmt.Fprintf(&b, "总语句覆盖 **%.1f%%**（%d/%d）", total.percent(), total.covered, total.total)
	if minTotal > 0 {
		fmt.Fprintf(&b, "，阈值 %.1f%%", minTotal)
	}
	b.WriteString("\n\n| 包 | 覆盖 | 语句 |\n|---|---:|---:|\n")
	for _, st := range stats {
		fmt.Fprintf(&b, "| `%s` | %.1f%% | %d/%d |\n", st.name, st.percent(), st.covered, st.total)
	}
	if len(failures) > 0 {
		b.WriteString("\n**门禁未通过：**\n\n")
		for _, f := range failures {
			fmt.Fprintf(&b, "- %s\n", f)
		}
	}
	b.WriteString("\n")

	f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open summary: %w", err)
	}
	if _, err := f.WriteString(b.String()); err != nil {
		_ = f.Close()
		return fmt.Errorf("write summary: %w", err)
	}
	return f.Close()
}
