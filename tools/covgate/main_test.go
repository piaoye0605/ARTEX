package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleProfile = `mode: set
github.com/x/y/server/a.go:1.1,2.2 10 1
github.com/x/y/server/a.go:3.1,4.2 30 0
github.com/x/y/db/b.go:1.1,2.2 60 1
`

func writeProfile(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "cover.out")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write profile: %v", err)
	}
	return p
}

func TestParseProfileDedupesRepeatedBlocks(t *testing.T) {
	// The same block shows up twice (two test packages, -coverpkg). The higher
	// count wins and the statement total is counted once.
	src := `mode: set
github.com/x/y/a.go:1.1,2.2 5 0
github.com/x/y/a.go:1.1,2.2 5 3
github.com/x/y/a.go:3.1,4.2 7 0
`
	p, err := parseProfile(strings.NewReader(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(p.blocks) != 2 {
		t.Fatalf("blocks=%d want 2", len(p.blocks))
	}
	total := p.total()
	if total.total != 12 || total.covered != 5 {
		t.Fatalf("total=%+v want 5/12", total)
	}
}

func TestParseProfileErrors(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"empty", "", "no mode line"},
		{"no mode", "github.com/x/y/a.go:1.1,2.2 1 1\n", "missing mode line"},
		{"dup mode", "mode: set\nmode: set\n", "duplicate mode"},
		{"one number", "mode: set\ngithub.com/x/y/a.go:1.1,2.2 1\n", "2 trailing numbers"},
		{"bad stmts", "mode: set\ngithub.com/x/y/a.go:1.1,2.2 x 1\n", "bad statement count"},
		{"bad count", "mode: set\ngithub.com/x/y/a.go:1.1,2.2 1 z\n", "bad hit count"},
		{"no range", "mode: set\ngithub.com/x/y/a.go 1 1\n", "bad block location"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseProfile(strings.NewReader(tc.src))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v want contains %q", err, tc.want)
			}
		})
	}
}

func TestByPackageGroupsByDirectory(t *testing.T) {
	p, err := parseProfile(strings.NewReader(sampleProfile))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	pkgs := p.byPackage()
	if len(pkgs) != 2 {
		t.Fatalf("pkgs=%d want 2", len(pkgs))
	}
	server := pkgs["github.com/x/y/server"]
	if server == nil || server.total != 40 || server.covered != 10 {
		t.Fatalf("server=%+v", server)
	}
	if got := server.percent(); got != 25 {
		t.Fatalf("server pct=%v want 25", got)
	}
	if got := p.total().percent(); got != 70 {
		t.Fatalf("total pct=%v want 70", got)
	}
}

func TestShortenPackages(t *testing.T) {
	full := map[string]*pkgStat{
		"github.com/x/y/server": {name: "github.com/x/y/server", covered: 1, total: 2},
		"github.com/x/y/db":     {name: "github.com/x/y/db", covered: 2, total: 2},
	}
	short := shortenPackages(full)
	if len(short) != 2 {
		t.Fatalf("short=%v", short)
	}
	if _, ok := short["server"]; !ok {
		t.Fatalf("missing server key: %v", short)
	}
	if short["db"].percent() != 100 {
		t.Fatalf("db pct=%v", short["db"].percent())
	}

	// A single package collapses to its base name rather than an empty key.
	one := shortenPackages(map[string]*pkgStat{
		"github.com/x/y/server": {name: "github.com/x/y/server", covered: 1, total: 1},
	})
	if _, ok := one["server"]; !ok {
		t.Fatalf("single package key lost: %v", one)
	}
}

func TestLookupPackageSuffixAndAmbiguity(t *testing.T) {
	full := map[string]*pkgStat{
		"github.com/x/y/cmd/artex":  {name: "github.com/x/y/cmd/artex"},
		"github.com/x/z/cmd/artex":  {name: "github.com/x/z/cmd/artex"},
		"github.com/x/y/server":     {name: "github.com/x/y/server"},
		"github.com/x/y/web/static": {name: "github.com/x/y/web/static"},
	}
	short := map[string]*pkgStat{"server": full["github.com/x/y/server"]}

	if st, ok := lookupPackage(short, full, "server"); !ok || st != short["server"] {
		t.Fatalf("short lookup failed")
	}
	if _, ok := lookupPackage(short, full, "static"); !ok {
		t.Fatalf("unique suffix lookup failed")
	}
	if _, ok := lookupPackage(short, full, "cmd/artex"); ok {
		t.Fatalf("ambiguous suffix must not resolve")
	}
	if _, ok := lookupPackage(short, full, "nope"); ok {
		t.Fatalf("unknown name must not resolve")
	}
}

func TestRunGate(t *testing.T) {
	p := writeProfile(t, sampleProfile)

	cases := []struct {
		name string
		args []string
		fail bool
	}{
		{"no gate", []string{"-profile", p}, false},
		{"total exactly at floor", []string{"-profile", p, "-min", "70"}, false},
		{"total below floor", []string{"-profile", p, "-min", "70.5"}, true},
		{"pkg at floor", []string{"-profile", p, "-pkg-min", "server=25"}, false},
		{"pkg below floor", []string{"-profile", p, "-pkg-min", "server=30"}, true},
		{"pkg full path", []string{"-profile", p, "-pkg-min", "github.com/x/y/db=100"}, false},
		{"pkg without data", []string{"-profile", p, "-pkg-min", "agent=1"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := run(tc.args, &out)
			if tc.fail {
				var gate *gateError
				if !errors.As(err, &gate) {
					t.Fatalf("err=%v want gate failure, out=%s", err, out.String())
				}
				if !strings.Contains(out.String(), "FAIL") {
					t.Fatalf("output missing FAIL: %s", out.String())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v (out=%s)", err, out.String())
			}
		})
	}
}

func TestRunRejectsBadFlags(t *testing.T) {
	p := writeProfile(t, sampleProfile)
	for _, args := range [][]string{
		{},
		{"-profile", p, "-min", "101"},
		{"-profile", p, "-pkg-min", "server"},
		{"-profile", p, "-pkg-min", "=5"},
		{"-profile", "/definitely/missing/cover.out"},
	} {
		var out bytes.Buffer
		if err := run(args, &out); err == nil {
			t.Fatalf("args=%v: expected error", args)
		} else {
			var gate *gateError
			if errors.As(err, &gate) {
				t.Fatalf("args=%v: input errors must not look like gate failures", args)
			}
		}
	}
}

func TestRunSummaryFile(t *testing.T) {
	p := writeProfile(t, sampleProfile)
	sum := filepath.Join(t.TempDir(), "summary.md")
	var out bytes.Buffer
	if err := run([]string{"-profile", p, "-min", "71", "-summary", sum, "-top", "1"}, &out); err == nil {
		t.Fatal("expected gate failure")
	}
	body, err := os.ReadFile(sum)
	if err != nil {
		t.Fatalf("read summary: %v", err)
	}
	text := string(body)
	for _, want := range []string{"### 覆盖率", "总语句覆盖 **70.0%**", "| `server` | 25.0% |", "门禁未通过"} {
		if !strings.Contains(text, want) {
			t.Fatalf("summary missing %q:\n%s", want, text)
		}
	}
	// -top 1 must trim the table, so the 100% package is not listed.
	if strings.Contains(text, "`db`") {
		t.Fatalf("summary should list only the worst package:\n%s", text)
	}
}

func TestRunQuietAndTopZero(t *testing.T) {
	p := writeProfile(t, sampleProfile)
	var out bytes.Buffer
	if err := run([]string{"-profile", p, "-quiet"}, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.Contains(out.String(), "最低的") {
		t.Fatalf("quiet output should only carry the total: %s", out.String())
	}

	out.Reset()
	if err := run([]string{"-profile", p, "-top", "0"}, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out.String(), "db") || !strings.Contains(out.String(), "server") {
		t.Fatalf("top=0 should list every package: %s", out.String())
	}
}

func TestRunMergesMultipleProfiles(t *testing.T) {
	a := writeProfile(t, "mode: set\ngithub.com/x/y/server/a.go:1.1,2.2 10 0\n")
	dir := t.TempDir()
	b := filepath.Join(dir, "b.out")
	if err := os.WriteFile(b, []byte("mode: set\ngithub.com/x/y/server/a.go:1.1,2.2 10 4\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	var out bytes.Buffer
	if err := run([]string{"-profile", a, "-profile", b}, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out.String(), "总语句覆盖 100.0%") {
		t.Fatalf("merged profile lost the covered run: %s", out.String())
	}
}

func TestBar(t *testing.T) {
	cases := map[float64]string{
		0:   "[..........]",
		4.9: "[..........]",
		5:   "[#.........]",
		100: "[##########]",
		120: "[##########]",
	}
	for pct, want := range cases {
		if got := bar(pct); got != want {
			t.Fatalf("bar(%v)=%q want %q", pct, got, want)
		}
	}
}

func TestRound1(t *testing.T) {
	if got := round1(72.96); got != 73.0 {
		t.Fatalf("round1(72.96)=%v", got)
	}
	if got := round1(72.94); got != 72.9 {
		t.Fatalf("round1(72.94)=%v", got)
	}
}
