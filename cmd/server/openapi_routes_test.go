package main

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
)

const modulePath = "github.com/sierragridteam/sierra-data"

// undocumentedRoutes are hand-mounted paths deliberately absent from the spec.
var undocumentedRoutes = map[string]string{
	"/":       "the embedded static site, not an API",
	"/livez":  "process health for the container check, not part of the data API",
	"/readyz": "read-path readiness for the container and load-balancer checks, not part of the data API",
}

// handRoute is one route mounted by hand rather than generated from grid.proto.
type handRoute struct {
	method  string // "" = every method (prefab.WithHTTPHandler*)
	pattern string
	at      string
}

// TestHandBuiltRoutesAreDocumented fails when the server mounts a route by hand
// (runtime.ServeMux.HandlePath or prefab.WithHTTPHandler[Func]) that the
// embedded OpenAPI spec does not describe. Generated GridService routes are in
// the spec by construction; hand-built ones are documented in
// api/grid/v1/openapi.overlay.yaml, and this is what keeps that file complete.
//
// It reads the source of cmd/server and every package of this module it
// imports, so a route added anywhere on the server's import graph is found, and
// the mesh relay (a separate binary) is not.
func TestHandBuiltRoutesAreDocumented(t *testing.T) {
	routes := findHandRoutes(t)
	if len(routes) < 4 {
		t.Fatalf("found only %d hand-mounted routes; the scanner is probably broken: %+v", len(routes), routes)
	}

	var spec struct {
		Paths map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(gridv1.OpenAPISpec, &spec); err != nil {
		t.Fatalf("parse embedded spec: %v", err)
	}

	for _, r := range routes {
		if _, skip := undocumentedRoutes[r.pattern]; skip {
			continue
		}
		item, ok := specPathFor(spec.Paths, r.pattern)
		if !ok {
			t.Errorf("%s mounts %s %s, which /api/openapi.json does not describe; add it under spec.paths in api/grid/v1/openapi.overlay.yaml and run `make openapi`",
				r.at, methodOrAny(r.method), r.pattern)
			continue
		}
		if r.method != "" {
			if _, ok := item[strings.ToLower(r.method)]; !ok {
				t.Errorf("%s mounts %s %s, but the spec has no %s operation on that path", r.at, r.method, r.pattern, strings.ToLower(r.method))
			}
		}
	}
}

func methodOrAny(m string) string {
	if m == "" {
		return "ANY"
	}
	return m
}

var pathParam = regexp.MustCompile(`\{[^}]*\}`)

// specPathFor finds the spec path documenting a mux pattern. Parameter names
// may differ, and a pattern ending in a parameter may be documented with a
// suffix on it: the map-layer route matches {layer} and documents
// {layer}.geojson.
func specPathFor(paths map[string]map[string]any, pattern string) (map[string]any, bool) {
	want := pathParam.ReplaceAllString(pattern, "{}")
	for p, item := range paths {
		got := pathParam.ReplaceAllString(p, "{}")
		if got == want || (strings.HasSuffix(want, "{}") && strings.HasPrefix(got, want+".")) {
			return item, true
		}
	}
	return nil, false
}

// findHandRoutes parses the non-test Go files of cmd/server and of every
// in-module package it (transitively) imports, and returns each hand-mounted
// route. A route whose method or path is not a string literal fails the test:
// it could not be checked.
func findHandRoutes(t *testing.T) []handRoute {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var routes []handRoute
	seen := map[string]bool{}
	queue := []string{filepath.Join(root, "cmd", "server")}
	for len(queue) > 0 {
		dir := queue[0]
		queue = queue[1:]
		if seen[dir] {
			continue
		}
		seen[dir] = true
		files := parseDir(t, fset, dir)
		consts := stringConsts(files)
		for _, f := range files {
			for _, imp := range f.Imports {
				path, _ := strconv.Unquote(imp.Path.Value)
				if rel, ok := strings.CutPrefix(path, modulePath+"/"); ok {
					queue = append(queue, filepath.Join(root, filepath.FromSlash(rel)))
				}
			}
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				at := fset.Position(call.Pos())
				at.Filename, _ = filepath.Rel(root, at.Filename)
				switch sel.Sel.Name {
				case "HandlePath":
					if len(call.Args) < 2 {
						return true
					}
					// A method that is not a constant (a loop over methods, as the
					// ingest route's 405 mounts are) is checked as "any method":
					// the path must still be documented.
					method, _ := resolveString(call.Args[0], consts)
					routes = append(routes, handRoute{
						method:  method,
						pattern: literal(t, call.Args[1], consts, at.String()),
						at:      at.String(),
					})
				case "WithHTTPHandlerFunc", "WithHTTPHandler":
					if len(call.Args) < 1 {
						return true
					}
					routes = append(routes, handRoute{pattern: literal(t, call.Args[0], consts, at.String()), at: at.String()})
				}
				return true
			})
		}
	}
	return routes
}

func parseDir(t *testing.T, fset *token.FileSet, dir string) []*ast.File {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, f)
	}
	return files
}

func literal(t *testing.T, e ast.Expr, consts map[string]string, at string) string {
	t.Helper()
	s, ok := resolveString(e, consts)
	if !ok {
		t.Errorf("%s: a hand-mounted route must name its path as a string literal or package constant so its spec coverage can be checked", at)
	}
	return s
}

// resolveString evaluates e when it is a string literal, a package-level
// string constant of the same package, or an http.MethodX constant.
func resolveString(e ast.Expr, consts map[string]string) (string, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind == token.STRING {
			s, err := strconv.Unquote(v.Value)
			return s, err == nil
		}
	case *ast.Ident:
		s, ok := consts[v.Name]
		return s, ok
	case *ast.SelectorExpr:
		if pkg, ok := v.X.(*ast.Ident); ok && pkg.Name == "http" {
			if m, ok := strings.CutPrefix(v.Sel.Name, "Method"); ok && m != "" {
				return strings.ToUpper(m), true
			}
		}
	}
	return "", false
}

// stringConsts collects the package-level string constants of files.
func stringConsts(files []*ast.File) map[string]string {
	consts := map[string]string{}
	for _, f := range files {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if i < len(vs.Values) {
						if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
							if s, err := strconv.Unquote(lit.Value); err == nil {
								consts[name.Name] = s
							}
						}
					}
				}
			}
		}
	}
	return consts
}
