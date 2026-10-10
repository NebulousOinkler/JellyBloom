// Package storagecheck enforces that JellyBloom stores no upstream files:
// outside the packages allowed below, no code may create, write, or rename
// files, or spool request bodies to disk. Persistent state goes through
// SQLite (the index database), which this check does not see.
package storagecheck

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// allowed are packages that may write files, and why.
var allowed = map[string]string{
	"internal/secrets": "stores upstream credentials (tokens), never upstream content",
	"testrig":          "test rig tools",
}

// forbidden lists file-writing functions by import path.
var forbidden = map[string][]string{
	"os":        {"Create", "CreateTemp", "WriteFile", "OpenFile", "MkdirTemp", "Rename", "Link", "Symlink", "Truncate"},
	"io/ioutil": {"WriteFile", "TempFile", "TempDir"},
}

// forbiddenMethods spool request bodies to temporary files.
var forbiddenMethods = []string{"ParseMultipartForm", "FormFile", "MultipartReader"}

func TestNoFileWrites(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") && rel != "." || rel == "testrig/data" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		for pkg := range allowed {
			if strings.HasPrefix(rel, pkg+"/") {
				return nil
			}
		}
		checked++
		for _, v := range violations(t, path) {
			t.Errorf("%s:%s (only %v may write files: JellyBloom never stores upstream content)", rel, v, keys(allowed))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 20 {
		t.Fatalf("only %d files checked; is the walk rooted at the module?", checked)
	}
}

func violations(t *testing.T, path string) []string {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	// Local names of the watched imports.
	names := map[string]string{}
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		if _, ok := forbidden[p]; !ok {
			continue
		}
		name := p[strings.LastIndex(p, "/")+1:]
		if imp.Name != nil {
			name = imp.Name.Name
		}
		names[name] = p
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok {
			if p, ok := names[id.Name]; ok {
				for _, fn := range forbidden[p] {
					if sel.Sel.Name == fn {
						out = append(out, strconv.Itoa(fset.Position(sel.Pos()).Line)+": "+p+"."+fn)
					}
				}
			}
		}
		for _, m := range forbiddenMethods {
			if sel.Sel.Name == m {
				out = append(out, strconv.Itoa(fset.Position(sel.Pos()).Line)+": "+m+" spools to temporary files")
			}
		}
		return true
	})
	return out
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// The check itself must catch violations.
func TestCheckFindsWrites(t *testing.T) {
	dir := t.TempDir()
	src := `package x
import (
	"net/http"
	fsys "os"
)
func f(r *http.Request) {
	fsys.WriteFile("x", nil, 0o600)
	r.ParseMultipartForm(1 << 20)
}`
	p := filepath.Join(dir, "x.go")
	if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if v := violations(t, p); len(v) != 2 {
		t.Errorf("violations = %v, want the aliased os.WriteFile and ParseMultipartForm", v)
	}
}
