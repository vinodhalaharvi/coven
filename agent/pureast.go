// Package agent: pureast tool.
//
// PureAstTool gives Claude AST-aware queries over Go source. It wraps
// the pureast library (https://github.com/Pure-Company/pureast) so
// every conversational agent can ask semantic questions like "list the
// methods on type Queries" or "extract User with its dependencies"
// without falling back to brittle text grep.
//
// The tool is part of agent.StandardTools — every agent gets it
// automatically. Pure tool, no y/N gate. Read-only by construction.
//
// Implementation: direct library calls, not subprocess. This means:
//   - no installation step for the user (pureast is a coven dependency)
//   - structured pkgNode data flows in-process (no CLI text parsing)
//   - failures surface as typed errors, not exit codes
//   - subprocess overhead per call is gone (~50ms each saved)
package agent

import (
	"context"
	"fmt"
	"go/token"
	"strings"

	"github.com/Pure-Company/pureast/pkg/analyze"
	astpkg "github.com/Pure-Company/pureast/pkg/ast"
	"github.com/Pure-Company/pureast/pkg/codegen"
	"github.com/Pure-Company/pureast/pkg/extract"
	"github.com/vinodhalaharvi/coven/llm"
)

// PureAstTool returns the pureast tool scoped to moduleRoot. Every
// path argument is resolved relative to moduleRoot via safeJoin, so
// agents can't escape the project even if asked to.
func PureAstTool(moduleRoot string) Tool {
	return Tool{
		Pure: true,
		Spec: llm.ToolSpec{
			Name: "pureast",
			Description: "Query Go source code semantically via AST analysis. Use when grepping text would be brittle — e.g. finding methods on a type, listing a package's API surface, extracting a symbol with all its dependencies, or diagnosing 'X references Y but Y has no field Z' drift.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"op": map[string]any{
						"type":        "string",
						"enum":        []string{"search", "list_symbols", "extract", "deps", "methods"},
						"description": "Query kind. search=fuzzy match symbol names. list_symbols=enumerate package contents. extract=symbol source + its transitive deps. deps=direct dependencies of a symbol. methods=methods defined on a type.",
					},
					"path": map[string]any{
						"type":        "string",
						"description": "Path (relative to project root) to the Go file or directory to analyze. Defaults to project root if omitted.",
					},
					"pattern": map[string]any{
						"type":        "string",
						"description": "For op=search: the symbol name pattern to fuzzy-match (e.g. 'Handler', 'User').",
					},
					"symbol": map[string]any{
						"type":        "string",
						"description": "For op=extract/deps/methods: the exact symbol name (type or function) to operate on.",
					},
					"kind": map[string]any{
						"type":        "string",
						"description": "For op=search: optional filter by kind. Common values: 'struct', 'interface', 'function'.",
					},
					"max_results": map[string]any{
						"type":        "integer",
						"description": "For op=search: maximum results returned (default 20).",
					},
					"minimal": map[string]any{
						"type":        "boolean",
						"description": "For op=extract: when true, include only direct dependencies, not the full transitive closure. Smaller output.",
					},
					"grouped": map[string]any{
						"type":        "boolean",
						"description": "For op=list_symbols: group output by symbol kind (default true).",
					},
				},
				"required": []string{"op"},
			},
		},
		Run: func(ctx context.Context, input map[string]any) (string, error) {
			return runPureAst(ctx, moduleRoot, input)
		},
	}
}

func runPureAst(ctx context.Context, moduleRoot string, input map[string]any) (string, error) {
	op, _ := input["op"].(string)
	if op == "" {
		return "", fmt.Errorf("op is required")
	}

	// Validate op-specific required args before any expensive parse work.
	switch op {
	case "search":
		if pattern, _ := input["pattern"].(string); pattern == "" {
			return "", fmt.Errorf("op=search requires 'pattern'")
		}
	case "list_symbols":
		// no extra required field
	case "extract", "deps", "methods":
		if sym, _ := input["symbol"].(string); sym == "" {
			return "", fmt.Errorf("op=%s requires 'symbol'", op)
		}
	default:
		return "", fmt.Errorf("unknown op %q (valid: search, list_symbols, extract, deps, methods)", op)
	}

	// Resolve path argument relative to module root, with the same
	// safeJoin guard the other tools use.
	relPath, _ := input["path"].(string)
	if relPath == "" {
		relPath = "."
	}
	abs, err := safeJoin(moduleRoot, relPath)
	if err != nil {
		return "", err
	}

	// Common preamble: parse the directory once, hand the resulting
	// pkgNode off to op-specific helpers. This is what every command
	// in pureast/cmd/pureast/commands/ does — the entry point is always
	// ExtractDirectoryConcurrent regardless of the eventual query.
	fset := token.NewFileSet()
	pkgNode, err := extract.ExtractDirectoryConcurrent(fset, abs, true, 0)
	if err != nil {
		return "", fmt.Errorf("pureast: extract %s: %w", abs, err)
	}

	var out string
	switch op {
	case "list_symbols":
		out = doListSymbols(pkgNode, input)
	case "search":
		out = doSearch(pkgNode, input)
	case "extract":
		out, err = doExtract(fset, pkgNode, input)
	case "deps":
		out, err = doDeps(pkgNode, input)
	case "methods":
		out, err = doMethods(pkgNode, input)
	}
	if err != nil {
		return "", err
	}

	// Cap output at 64KB to protect Claude's context. Some queries
	// (extract on big packages, list on entire projects) can otherwise
	// produce hundreds of KB.
	const maxOutput = 64 * 1024
	if len(out) > maxOutput {
		return out[:maxOutput] + fmt.Sprintf("\n... (truncated; %d bytes total)", len(out)), nil
	}
	if strings.TrimSpace(out) == "" {
		return "(no output)", nil
	}
	return out, nil
}

// doListSymbols enumerates the package and formats by kind.
//
// Mirrors cmd/pureast/commands/list.go: DiscoverAllSymbols then
// FormatSymbolList with the grouped flag.
func doListSymbols(pkgNode astpkg.PackageNode, input map[string]any) string {
	symbols := extract.DiscoverAllSymbols(pkgNode)
	grouped := true
	if v, ok := input["grouped"].(bool); ok {
		grouped = v
	}
	header := fmt.Sprintf("Found %d symbols in package %q\n", len(symbols), pkgNode.Name)
	return header + extract.FormatSymbolList(symbols, grouped)
}

// doSearch fuzzy-matches symbol names with optional kind filter.
//
// Mirrors cmd/pureast/commands/search.go: DiscoverAllSymbols then
// FuzzySearch with pattern, kind, and max-results.
func doSearch(pkgNode astpkg.PackageNode, input map[string]any) string {
	symbols := extract.DiscoverAllSymbols(pkgNode)
	pattern, _ := input["pattern"].(string)
	kind, _ := input["kind"].(string)
	maxResults := 20
	switch v := input["max_results"].(type) {
	case int:
		maxResults = v
	case float64: // JSON numbers come through as float64
		maxResults = int(v)
	}

	matches := extract.FuzzySearch(symbols, pattern, kind, maxResults)
	var b strings.Builder
	fmt.Fprintf(&b, "Found %d matches for %q:\n\n", len(matches), pattern)
	for i, m := range matches {
		fmt.Fprintf(&b, "%d. %s (%s) [score: %d]\n", i+1, m.Symbol.Name, m.Symbol.Kind, m.Score)
	}
	return b.String()
}

// doExtract pulls out a symbol with its transitive dependencies as
// compilable Go code.
//
// Mirrors cmd/pureast/commands/extract.go: BuildPackageDeclMap +
// NewDependencyGraph + GenerateMinimal. The minimal flag toggles
// between MinimalDependencies (direct only) and ResolveWithAssociatedCode
// (transitive + constructors + methods).
func doExtract(fset *token.FileSet, pkgNode astpkg.PackageNode, input map[string]any) (string, error) {
	sym, _ := input["symbol"].(string)
	declMap := extract.BuildPackageDeclMap(pkgNode)
	graph := analyze.NewDependencyGraph(declMap)

	var deps astpkg.Dependencies
	if minimal, _ := input["minimal"].(bool); minimal {
		deps = graph.MinimalDependencies(sym)
	} else {
		deps = graph.ResolveWithAssociatedCode(sym)
	}

	gen := codegen.NewGenerator(fset)
	code, err := gen.GenerateMinimal(pkgNode.Name, sym, declMap, deps)
	if err != nil {
		return "", fmt.Errorf("pureast: generate %s: %w", sym, err)
	}
	return code, nil
}

// doDeps lists a symbol's direct dependencies, grouped by kind.
//
// Dependencies in pureast is a struct with seven SetMonoid[string]
// fields (Types, Functions, Structs, Interfaces, Imports, Constants,
// Variables). Each set exposes ToSlice() for iteration. We render each
// non-empty group as its own section.
func doDeps(pkgNode astpkg.PackageNode, input map[string]any) (string, error) {
	sym, _ := input["symbol"].(string)
	declMap := extract.BuildPackageDeclMap(pkgNode)
	graph := analyze.NewDependencyGraph(declMap)
	deps := graph.MinimalDependencies(sym)

	groups := []struct {
		label string
		items []string
	}{
		{"Types", sortedSlice(deps.Types.ToSlice())},
		{"Structs", sortedSlice(deps.Structs.ToSlice())},
		{"Interfaces", sortedSlice(deps.Interfaces.ToSlice())},
		{"Functions", sortedSlice(deps.Functions.ToSlice())},
		{"Constants", sortedSlice(deps.Constants.ToSlice())},
		{"Variables", sortedSlice(deps.Variables.ToSlice())},
		{"Imports", sortedSlice(deps.Imports.ToSlice())},
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Direct dependencies of %s:\n", sym)
	total := 0
	for _, g := range groups {
		if len(g.items) == 0 {
			continue
		}
		total += len(g.items)
		fmt.Fprintf(&b, "  %s:\n", g.label)
		for _, name := range g.items {
			fmt.Fprintf(&b, "    - %s\n", name)
		}
	}
	if total == 0 {
		b.WriteString("  (none)\n")
	}
	return b.String(), nil
}

// sortedSlice returns a copy of input sorted alphabetically. Stable
// output makes test assertions and diff inspection cleaner.
func sortedSlice(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	// Simple insertion sort; lists are tiny.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// doMethods lists methods defined on the named type.
//
// Filters the package's symbol catalog to entries with Kind == "method"
// and Receiver matching the requested type. The Receiver field is the
// type name without pointer indirection (e.g. "User" matches both
// "func (u User)..." and "func (u *User)...").
func doMethods(pkgNode astpkg.PackageNode, input map[string]any) (string, error) {
	sym, _ := input["symbol"].(string)
	all := extract.DiscoverAllSymbols(pkgNode)

	var methods []extract.SymbolInfo
	for _, s := range all {
		if s.Kind != "method" {
			continue
		}
		if s.Receiver == sym {
			methods = append(methods, s)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Methods on %s:\n", sym)
	if len(methods) == 0 {
		b.WriteString("(no methods found; the type may not exist in this package or have no methods)\n")
		return b.String(), nil
	}
	for _, m := range methods {
		fmt.Fprintf(&b, "  - %s\n", m.Name)
	}
	return b.String(), nil
}
