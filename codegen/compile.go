package codegen

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"log"
	"path/filepath"
	"strings"

	"github.com/lukechampine/ply/importer"
	"github.com/lukechampine/ply/types"

	"github.com/tsuna/gorewrite"
	"golang.org/x/tools/go/ast/astutil"
)

// A specializer is a Rewriter that generates specialized versions of each
// generic ply function and rewrites the callsites to use their corresponding
// specialized function.
type specializer struct {
	types   map[ast.Expr]types.TypeAndValue
	fset    *token.FileSet
	pkg     *ast.Package
	imports map[string]struct{}
}

func hasMethod(recv ast.Expr, method string, exprTypes map[ast.Expr]types.TypeAndValue) bool {
	// TODO: use set.Lookup instead of searching manually
	set := types.NewMethodSet(exprTypes[recv].Type)
	for i := 0; i < set.Len(); i++ {
		if set.At(i).Obj().(*types.Func).Name() == method {
			return true
		}
	}
	return false
}

func (s specializer) addDecl(filename, code string) {
	if _, ok := s.pkg.Files[filename]; ok {
		// check for existence first, because parsing is expensive
		return
	}
	// add package header to code
	code = "package " + s.pkg.Name + code
	f, err := parser.ParseFile(s.fset, "", code, 0)
	if err != nil {
		log.Fatal(err)
	}
	s.pkg.Files[filename] = f
}

func (s specializer) Rewrite(node ast.Node) (ast.Node, gorewrite.Rewriter) {
	switch n := node.(type) {
	case *ast.CallExpr:
		var rewrote bool
		switch fn := n.Fun.(type) {
		case *ast.Ident:
			if gen, ok := funcGenerators[fn.Name]; ok {
				if v := s.types[n].Value; v != nil {
					// some functions (namely max/min) may evaluate to a
					// constant, in which case we should replace the call with
					// a constant expression.
					node = ast.NewIdent(v.ExactString())
				} else {
					name, code, rewrite := gen(fn, n.Args, s.types)
					s.addDecl(name, code)
					node = rewrite(n)
					rewrote = true
				}
			}

		case *ast.SelectorExpr:
			// Detect and construct a pipeline if possible. Otherwise,
			// generate a single method.
			var chain []*ast.CallExpr
			cur := n
			for ok := true; ok; cur, ok = cur.Fun.(*ast.SelectorExpr).X.(*ast.CallExpr) {
				if _, ok := cur.Fun.(*ast.SelectorExpr); !ok {
					break
				}
				chain = append(chain, cur)
			}
			if p := buildPipeline(chain, s.types); p != nil {
				name, code, rewrite := p.gen()
				s.addDecl(name, code)
				node = rewrite(n)
				rewrote = true
			} else if gen, ok := methodGenerators[fn.Sel.Name]; ok && !hasMethod(fn.X, fn.Sel.Name, s.types) {
				name, code, rewrite := gen(fn, n.Args, s.types)
				s.addDecl(name, code)
				node = rewrite(n)
				if fn.Sel.Name == "sort" {
					s.imports["sort"] = struct{}{}
				}
				rewrote = true
			}
		}
		if named, ok := s.types[n].Type.(*types.Named); ok && rewrote {
			// if we rewrote a callsite that returns a named type, cast the
			// expression to the named type directly to prevent the incorrect
			// type from being inferred
			node = &ast.CallExpr{
				Fun:  ast.NewIdent(named.String()),
				Args: []ast.Expr{node.(ast.Expr)},
			}
		}
	}
	return node, s
}

func astToBytes(fset *token.FileSet, node interface{}) []byte {
	var buf bytes.Buffer
	pcfg := &printer.Config{Tabwidth: 8, Mode: printer.RawFormat | printer.SourcePos}
	pcfg.Fprint(&buf, fset, node)
	return buf.Bytes()
}

// Compile compiles the provided files as a single package. For each supplied
// .ply file, the compiled Go code is returned, keyed by a suggested filename
// (not a full filepath).
func Compile(filenames []string) (map[string][]byte, error) {
	// parse each supplied file
	fset := token.NewFileSet()
	var files []*ast.File
	plyFiles := make(map[string]*ast.File)
	for _, arg := range filenames {
		f, err := parser.ParseFile(fset, arg, nil, parser.ParseComments)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
		if filepath.Ext(arg) == ".ply" {
			plyFiles[arg] = f
		}
	}
	if len(plyFiles) == 0 {
		return nil, nil
	}

	// type-check the package
	info := types.Info{
		Types: make(map[ast.Expr]types.TypeAndValue),
	}
	var conf types.Config
	conf.Importer = importer.Default()
	pkg, err := conf.Check("", fset, files, &info)
	if err != nil {
		return nil, err
	}

	// walk the AST of each .ply file in the package, generating ply functions
	// and rewriting their callsites
	spec := specializer{
		types: info.Types,
		fset:  fset,
		pkg: &ast.Package{
			Name:  pkg.Name(),
			Files: make(map[string]*ast.File),
		},
		imports: make(map[string]struct{}),
	}
	for _, f := range plyFiles {
		gorewrite.Rewrite(spec, f)
	}

	// write compiled files to set
	set := make(map[string][]byte)
	for name, f := range plyFiles {
		name = "ply-" + strings.Replace(filepath.Base(name), ".ply", ".go", -1)
		set[name] = astToBytes(fset, f)
	}

	// combine generated ply functions into a single file
	merged := ast.MergePackageFiles(spec.pkg, ast.FilterFuncDuplicates|ast.FilterImportDuplicates)
	// add imports
	for importPath := range spec.imports {
		astutil.AddImport(fset, merged, importPath)
	}

	// add ply-impls to set
	set["ply-impls.go"] = astToBytes(fset, merged)

	return set, nil
}
