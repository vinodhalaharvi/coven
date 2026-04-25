package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ChecksumDir computes a SHA-256 over the concatenated contents of all .go
// files in dir (non-recursive). Order is deterministic (alphabetical).
func ChecksumDir(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		files = append(files, e.Name())
	}
	sort.Strings(files)

	h := sha256.New()
	for _, name := range files {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s\n", name)
		h.Write(data)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ExtractExports parses all .go files in dir (non-recursive, skipping _test.go)
// and returns exported top-level declarations.
func ExtractExports(dir string) ([]Symbol, error) {
	syms, _, err := parseDir(dir)
	return syms, err
}

// ExtractImports parses all .go files in dir (non-recursive, skipping
// _test.go) and returns the de-duplicated set of imported package paths.
func ExtractImports(dir string) ([]PackageID, error) {
	_, imps, err := parseDir(dir)
	return imps, err
}

// ExtractAll runs the AST walk once and returns both exports and imports.
// Used by the agent to avoid parsing twice in the per-event program.
func ExtractAll(dir string) ([]Symbol, []PackageID, error) {
	return parseDir(dir)
}

func parseDir(dir string) ([]Symbol, []PackageID, error) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		return nil, nil, err
	}

	importSet := map[string]struct{}{}
	var syms []Symbol
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, imp := range f.Imports {
				if imp.Path != nil {
					p := strings.Trim(imp.Path.Value, `"`)
					if p != "" {
						importSet[p] = struct{}{}
					}
				}
			}
			for _, decl := range f.Decls {
				switch d := decl.(type) {
				case *ast.FuncDecl:
					if !d.Name.IsExported() {
						continue
					}
					if d.Recv != nil {
						continue
					}
					syms = append(syms, Symbol{
						Name:      d.Name.Name,
						Kind:      "func",
						Signature: formatFuncSig(d),
					})
				case *ast.GenDecl:
					for _, spec := range d.Specs {
						switch s := spec.(type) {
						case *ast.TypeSpec:
							if s.Name.IsExported() {
								syms = append(syms, Symbol{
									Name: s.Name.Name, Kind: "type",
									Signature: s.Name.Name,
								})
							}
						case *ast.ValueSpec:
							kind := "var"
							if d.Tok == token.CONST {
								kind = "const"
							}
							for _, n := range s.Names {
								if n.IsExported() {
									syms = append(syms, Symbol{
										Name: n.Name, Kind: kind, Signature: n.Name,
									})
								}
							}
						}
					}
				}
			}
		}
	}
	sort.Slice(syms, func(i, j int) bool { return syms[i].Name < syms[j].Name })

	imps := make([]PackageID, 0, len(importSet))
	for p := range importSet {
		imps = append(imps, PackageID(p))
	}
	sort.Slice(imps, func(i, j int) bool { return imps[i] < imps[j] })

	return syms, imps, nil
}

func formatFuncSig(fn *ast.FuncDecl) string {
	var b strings.Builder
	b.WriteString(fn.Name.Name)
	b.WriteString("(")
	if fn.Type.Params != nil {
		for i, p := range fn.Type.Params.List {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(exprString(p.Type))
		}
	}
	b.WriteString(")")
	if fn.Type.Results != nil && len(fn.Type.Results.List) > 0 {
		b.WriteString(" ")
		if len(fn.Type.Results.List) > 1 {
			b.WriteString("(")
		}
		for i, r := range fn.Type.Results.List {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(exprString(r.Type))
		}
		if len(fn.Type.Results.List) > 1 {
			b.WriteString(")")
		}
	}
	return b.String()
}

func exprString(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return "*" + exprString(t.X)
	case *ast.SelectorExpr:
		return exprString(t.X) + "." + t.Sel.Name
	case *ast.ArrayType:
		return "[]" + exprString(t.Elt)
	case *ast.MapType:
		return "map[" + exprString(t.Key) + "]" + exprString(t.Value)
	case *ast.InterfaceType:
		return "interface{}"
	case *ast.Ellipsis:
		return "..." + exprString(t.Elt)
	default:
		return "?"
	}
}

// RunGoBuild executes `go build ./...` in dir.
func RunGoBuild(ctx context.Context, dir string) BuildResult {
	start := time.Now()
	cmd := exec.CommandContext(ctx, "go", "build", "./...")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return BuildResult{
		OK:       err == nil,
		Output:   string(out),
		Duration: time.Since(start),
	}
}

// RunGoVet executes `go vet ./...` in dir.
func RunGoVet(ctx context.Context, dir string) VetResult {
	cmd := exec.CommandContext(ctx, "go", "vet", "./...")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return VetResult{
		Clean:  err == nil,
		Output: string(out),
	}
}
