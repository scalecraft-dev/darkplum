// Package lint holds structural tests over the CLI's own source. They are tests,
// not a linter binary, so `go test ./...` (and CI) cannot skip them.
package lint

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// kubeNames are commands that must only ever run through a cluster handle. Only
// internal/runner may spell them as string literals; everyone else uses
// runner.Kubectl.
var kubeNames = map[string]bool{"kubectl": true, "helm": true}

// execNames are every program the CLI shells out to.
var execNames = map[string]bool{"kubectl": true, "helm": true, "aws": true, "terraform": true}

// check returns the violations in one parsed non-test file. rel is the slash
// path of the file relative to the CLI module root.
func check(fset *token.FileSet, f *ast.File, rel string) []string {
	if strings.HasPrefix(rel, "internal/runner/") {
		return nil
	}
	var out []string
	pos := func(n ast.Node) string { return fmt.Sprintf("%s:%d", rel, fset.Position(n.Pos()).Line) }

	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		if p == "os/exec" {
			out = append(out, pos(imp)+`: imports "os/exec": only internal/runner may start external programs`)
		}
	}

	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.BasicLit:
			if x.Kind == token.STRING {
				if s, err := strconv.Unquote(x.Value); err == nil && kubeNames[s] {
					out = append(out, pos(x)+fmt.Sprintf(": string literal %q outside internal/runner (use runner.Kubectl and a cluster handle)", s))
				}
			}
		case *ast.SelectorExpr:
			// kube.ForTest takes a fake runner; in production code it would let a
			// client skip the handle.
			if id, ok := x.X.(*ast.Ident); ok && id.Name == "kube" && x.Sel.Name == "ForTest" {
				out = append(out, pos(x)+": kube.ForTest is for _test.go files only")
			}
		case *ast.AssignStmt:
			// Swapping a client's runner after construction retargets it.
			for _, lhs := range x.Lhs {
				if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "Runner" {
					out = append(out, pos(x)+": assignment to a .Runner field: clients are built by kube.ForCluster or kube.ForLocalKind and never retargeted")
				}
			}
		case *ast.CompositeLit:
			// A kube.Client built by hand skips ForCluster/ForLocalKind, so skips the handle.
			if sel, ok := x.Type.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "kube" && sel.Sel.Name == "Client" && !strings.HasPrefix(rel, "internal/kube/") {
					out = append(out, pos(x)+": kube.Client built by hand: use kube.ForCluster or kube.ForLocalKind")
				}
				// A handle written as a literal is the zero value, which is inert, but
				// it is never what code means: handles come from Validate / NewLocalKind.
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "runner" && (sel.Sel.Name == "ValidatedCluster" || sel.Sel.Name == "LocalKind") {
					out = append(out, pos(x)+": runner."+sel.Sel.Name+" built by hand: handles come from runner.Validate or runner.NewLocalKind")
				}
			}
		}
		return true
	})
	return out
}

func parse(t *testing.T, name, src string) (*token.FileSet, *ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	return fset, f
}

// cliRoot is services/cli, two levels above this package.
func cliRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("cannot find the CLI module root from %s: %v", root, err)
	}
	return root
}

// TestNoExecOutsideRunner is the structural guarantee behind "kubectl and aws
// only run against the validated install": no production file outside
// internal/runner imports os/exec, names kubectl or helm, or builds a kube.Client
// without a handle. Test files are exempt (they fake programs on PATH).
func TestNoExecOutsideRunner(t *testing.T) {
	root := cliRoot(t)
	var violations []string
	files := 0
	for _, dir := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			rel = filepath.ToSlash(rel)
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, p, nil, parser.ParseComments)
			if err != nil {
				return err
			}
			files++
			violations = append(violations, check(fset, f, rel)...)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if files < 20 {
		t.Fatalf("lint walked only %d files: wrong root?", files)
	}
	sort.Strings(violations)
	for _, v := range violations {
		t.Error(v)
	}
}

// The checker has to actually catch the things it claims to. Each case is source
// a future change might contain.
func TestCheckerCatchesViolations(t *testing.T) {
	cases := map[string]struct {
		rel, src string
		want     int
	}{
		"os/exec import":  {"internal/commands/x.go", "package commands\nimport \"os/exec\"\nvar _ = exec.Command\n", 1},
		"aliased os/exec": {"internal/commands/x.go", "package commands\nimport sh \"os/exec\"\nvar _ = sh.Command\n", 1},
		"kubectl literal": {"internal/commands/x.go", "package commands\nvar c = []string{\"kubectl\", \"get\"}\n", 1},
		"helm literal":    {"internal/commands/x.go", "package commands\nfunc f(r interface{ Run(...string) }) { r.Run(\"helm\", \"ls\") }\n", 1},
		"hand-built kube.Client": {"internal/commands/x.go",
			"package commands\nimport \"x/kube\"\nvar _ = &kube.Client{Namespace: \"n\"}\n", 1},
		"exec and kubectl both":      {"cmd/x.go", "package main\nimport \"os/exec\"\nvar _ = exec.Command(\"kubectl\")\n", 2},
		"kube.ForTest in production": {"internal/commands/x.go", "package commands\nimport \"x/kube\"\nvar _ = kube.ForTest\n", 1},
		"runner swap by assignment":  {"internal/commands/x.go", "package commands\nfunc f(c *struct{ Runner int }) { c.Runner = 1 }\n", 1},
		"hand built handle":          {"internal/commands/x.go", "package commands\nimport \"x/runner\"\nvar _ = runner.LocalKind{}\nvar _ = runner.ValidatedCluster{}\n", 2},
		"clean file":                 {"internal/commands/x.go", "package commands\nimport \"fmt\"\nvar _ = fmt.Sprint(\"aws\", \"terraform\")\n", 0},
		"runner may do anything":     {"internal/runner/x.go", "package runner\nimport \"os/exec\"\nvar _ = exec.Command(\"kubectl\")\n", 0},
		"kube package may build its own client": {"internal/kube/x.go",
			"package kube\nvar _ = &Client{}\n", 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fset, f := parse(t, "x.go", tc.src)
			if got := check(fset, f, tc.rel); len(got) != tc.want {
				t.Errorf("violations = %v, want %d", got, tc.want)
			}
		})
	}
}

// A literal use of the tool names that are fine in commands is not flagged (aws
// and terraform go through runner.Tool, which refuses anything else), but a call
// path that would skip it is the import ban's job. This pins that split so
// nobody widens one rule thinking the other covers it.
func TestAwsAndTerraformLiteralsAreAllowedOutsideRunner(t *testing.T) {
	fset, f := parse(t, "x.go", "package commands\nvar _ = []string{\"aws\", \"terraform\"}\n")
	if got := check(fset, f, "internal/commands/x.go"); len(got) != 0 {
		t.Errorf("aws/terraform literals flagged: %v", got)
	}
	if !execNames["aws"] || !execNames["terraform"] {
		t.Error("execNames lost a tool")
	}
}
