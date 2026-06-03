package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/importer"
	"go/parser"
	"go/printer"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/tools/go/packages"
)

type pairFlag []string

func (p *pairFlag) String() string {
	return strings.Join(*p, ",")
}

func (p *pairFlag) Set(value string) error {
	if !strings.Contains(value, ":") {
		return fmt.Errorf("pair must be Src:Dst, got %q", value)
	}
	*p = append(*p, value)
	return nil
}

type model struct {
	pkgName    string
	pkgPath    string
	pairs      []copyPair
	imports    map[string]string
	issues     []string
	converters map[string]staticConverter
}

type generationError struct {
	err error
}

func (e generationError) Error() string {
	return e.err.Error()
}

type copyPair struct {
	srcName      string
	dstName      string
	srcType      types.Type
	dstType      types.Type
	src          *types.Struct
	dst          *types.Struct
	fromPtr      bool
	fields       []fieldCopy
	converters   map[string]staticConverter
	sourceFile   string
	discoveredAt string
}

type fieldCopy struct {
	srcName     string
	srcExpr     string
	dstName     string
	srcType     types.Type
	dstType     types.Type
	flags       uint8
	zeroExpr    string
	assignExpr  string
	nilSafe     bool
	zeroAssign  string
	nested      []fieldCopy
	converter   bool
	converterFn string
	insensitive bool
	importTypes []types.Type
}

type staticConverter struct {
	srcType types.Type
	dstType types.Type
	fn      string
}

const (
	tagIgnore uint8 = 1 << iota
	tagOverride
	tagMust
)

func main() {
	var pairs pairFlag
	out := flag.String("out", "", "generated file path; defaults to <source>_copier_gen.go per source file")
	dir := flag.String("dir", ".", "package directory")
	flag.Var(&pairs, "pair", "copy pair as Src:Dst; optional, generator also scans copier.Copy calls")
	flag.Parse()

	m, err := loadModel(*dir, pairs)
	if err != nil {
		fatalf("%v", err)
	}
	if len(m.pairs) == 0 {
		if len(m.issues) > 0 {
			fatalf("found copier calls, but could not generate mappers:\n%s", strings.Join(m.issues, "\n"))
		}
		fatalf("no copier.Copy or copier.CopyWithOption calls found; pass -pair Src:Dst for an explicit mapper")
	}

	if err := writeGeneratedFiles(*dir, *out, m); err != nil {
		fatalf("%v", err)
	}
}

func loadModel(dir string, rawPairs []string) (model, error) {
	if m, err := loadModelPackages(dir, rawPairs); err == nil {
		if len(m.pairs) > 0 || len(rawPairs) > 0 {
			return m, nil
		}
		fallback, fallbackErr := loadModelStd(dir, rawPairs)
		if fallbackErr != nil {
			return fallback, fallbackErr
		}
		fallback.issues = append(m.issues, fallback.issues...)
		return fallback, nil
	} else {
		var genErr generationError
		if errors.As(err, &genErr) {
			return model{}, genErr.err
		}
	}
	return loadModelStd(dir, rawPairs)
}

func loadModelPackages(dir string, rawPairs []string) (model, error) {
	cfg := &packages.Config{
		Mode: packages.NeedName |
			packages.NeedFiles |
			packages.NeedSyntax |
			packages.NeedTypes |
			packages.NeedTypesInfo |
			packages.NeedImports,
		Dir:   dir,
		Tests: false,
	}
	pkgs, err := packages.Load(cfg, ".")
	if err != nil {
		return model{}, err
	}
	if len(pkgs) == 0 || pkgs[0].Types == nil || pkgs[0].TypesInfo == nil {
		return model{}, fmt.Errorf("package type information is unavailable")
	}
	pkg := pkgs[0]
	m := model{pkgName: pkg.Name, pkgPath: pkg.PkgPath, imports: map[string]string{}, converters: map[string]staticConverter{}}
	for _, err := range pkg.Errors {
		m.issues = append(m.issues, err.Error())
	}
	m.converters = discoverStaticConverters(pkg.Fset, pkg.Syntax, pkg.TypesInfo)
	seen := map[string]bool{}
	discovered, issues := discoverCopyPairs(pkg.Fset, pkg.Syntax, pkg.TypesInfo, pkg.Types)
	m.issues = append(m.issues, issues...)
	for _, pair := range discovered {
		key := typeKey(pair.srcType) + "->" + typeKey(pair.dstType)
		if seen[key] {
			continue
		}
		seen[key] = true
		built, err := buildPairFromTypes(pair, pkg.PkgPath, mergeConverters(m.converters, pair.converters))
		if err != nil {
			return model{}, generationError{err: err}
		}
		m.pairs = append(m.pairs, built)
	}
	for _, raw := range rawPairs {
		parts := strings.SplitN(raw, ":", 2)
		pair, err := buildPair(pkg.Types, strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), m.converters)
		if err != nil {
			return model{}, err
		}
		key := typeKey(pair.srcType) + "->" + typeKey(pair.dstType)
		if !seen[key] {
			seen[key] = true
			m.pairs = append(m.pairs, pair)
		}
	}
	return m, nil
}

func loadModelStd(dir string, rawPairs []string) (model, error) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return model{}, err
	}

	var files []*ast.File
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || strings.HasSuffix(name, "_gen.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ParseComments)
		if err != nil {
			return model{}, err
		}
		files = append(files, file)
	}
	if len(files) == 0 {
		return model{}, fmt.Errorf("no Go files found in %s", dir)
	}

	conf := types.Config{
		Importer: importer.Default(),
		Error:    func(error) {},
	}
	info := &types.Info{
		Types: make(map[ast.Expr]types.TypeAndValue),
		Defs:  make(map[*ast.Ident]types.Object),
		Uses:  make(map[*ast.Ident]types.Object),
	}
	pkg, err := conf.Check(files[0].Name.Name, fset, files, info)
	if pkg == nil {
		return model{}, err
	}

	m := model{pkgName: pkg.Name(), pkgPath: pkg.Path(), imports: map[string]string{}, converters: map[string]staticConverter{}}
	m.converters = discoverStaticConverters(fset, files, info)
	seen := map[string]bool{}
	discovered, issues := discoverCopyPairs(fset, files, info, pkg)
	m.issues = append(m.issues, issues...)
	for _, pair := range discovered {
		key := typeKey(pair.srcType) + "->" + typeKey(pair.dstType)
		if seen[key] {
			continue
		}
		seen[key] = true
		built, err := buildPairFromTypes(pair, pkg.Path(), mergeConverters(m.converters, pair.converters))
		if err != nil {
			return model{}, err
		}
		m.pairs = append(m.pairs, built)
	}

	for _, raw := range rawPairs {
		parts := strings.SplitN(raw, ":", 2)
		pair, err := buildPair(pkg, strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), m.converters)
		if err != nil {
			return model{}, err
		}
		key := typeKey(pair.srcType) + "->" + typeKey(pair.dstType)
		if !seen[key] {
			seen[key] = true
			m.pairs = append(m.pairs, pair)
		}
	}
	return m, nil
}

func discoverCopyPairs(fset *token.FileSet, files []*ast.File, info *types.Info, pkg *types.Package) ([]copyPair, []string) {
	var pairs []copyPair
	var issues []string
	for _, file := range files {
		explicitTypes := collectExplicitTypes(file, pkg)
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) < 2 {
				return true
			}
			if !isCopierCall(file, call, info) {
				return true
			}
			pos := fset.Position(call.Lparen)
			sourceFile := filepath.Base(pos.Filename)
			location := fmt.Sprintf("%s:%d", filepath.Base(pos.Filename), pos.Line)
			dstType := copyExpressionType(info, file, pkg, explicitTypes, call.Args[0])
			srcType := copyExpressionType(info, file, pkg, explicitTypes, call.Args[1])
			dstNamed, _, ok := namedStructFromCopyArg(dstType)
			if !ok {
				issues = append(issues, fmt.Sprintf("%s: destination argument is not a pointer to named struct: %s", location, typeKeyOrUnknown(dstType)))
				return true
			}
			srcNamed, fromPtr, ok := namedStructFromCopyArg(srcType)
			if !ok {
				issues = append(issues, fmt.Sprintf("%s: source argument is not a named struct or pointer to named struct: %s", location, typeKeyOrUnknown(srcType)))
				return true
			}
			pairConverters := map[string]staticConverter{}
			if len(call.Args) >= 3 {
				pairConverters = discoverOptionConverters(fset, file, info, call.Args[2])
			}
			pairs = append(pairs, copyPair{
				srcName:      srcNamed.Obj().Name(),
				dstName:      dstNamed.Obj().Name(),
				srcType:      srcNamed,
				dstType:      dstNamed,
				src:          srcNamed.Underlying().(*types.Struct),
				dst:          dstNamed.Underlying().(*types.Struct),
				fromPtr:      fromPtr,
				converters:   pairConverters,
				sourceFile:   sourceFile,
				discoveredAt: fmt.Sprintf("%s:%d", filepath.Base(pos.Filename), pos.Line),
			})
			_ = pkg
			return true
		})
	}
	return pairs, issues
}

func discoverStaticConverters(fset *token.FileSet, files []*ast.File, info *types.Info) map[string]staticConverter {
	converters := map[string]staticConverter{}
	for _, file := range files {
		for _, decl := range file.Decls {
			genDecl, ok := decl.(*ast.GenDecl)
			if !ok || genDecl.Tok != token.VAR {
				continue
			}
			for _, spec := range genDecl.Specs {
				valueSpec, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, value := range valueSpec.Values {
					call, ok := value.(*ast.CallExpr)
					if !ok {
						continue
					}
					converter, ok := staticConverterFromUseCall(fset, file, info, call)
					if !ok {
						continue
					}
					converters[converterKey(converter.srcType, converter.dstType)] = converter
				}
			}
		}
	}
	return converters
}

func discoverOptionConverters(fset *token.FileSet, file *ast.File, info *types.Info, expr ast.Expr) map[string]staticConverter {
	converters := map[string]staticConverter{}
	for _, converter := range convertersFromExpr(fset, file, info, expr) {
		converters[converterKey(converter.srcType, converter.dstType)] = converter
	}
	return converters
}

func convertersFromExpr(fset *token.FileSet, file *ast.File, info *types.Info, expr ast.Expr) []staticConverter {
	var converters []staticConverter
	switch e := expr.(type) {
	case *ast.CompositeLit:
		for _, elt := range e.Elts {
			switch kv := elt.(type) {
			case *ast.KeyValueExpr:
				key, ok := kv.Key.(*ast.Ident)
				if !ok || key.Name != "Converters" {
					continue
				}
				converters = append(converters, convertersFromExpr(fset, file, info, kv.Value)...)
			default:
				converters = append(converters, convertersFromExpr(fset, file, info, elt)...)
			}
		}
	case *ast.CallExpr:
		converter, ok := staticConverterFromUseCall(fset, file, info, e)
		if ok {
			converters = append(converters, converter)
		}
	}
	return converters
}

func staticConverterFromUseCall(fset *token.FileSet, file *ast.File, info *types.Info, call *ast.CallExpr) (staticConverter, bool) {
	if len(call.Args) != 1 {
		return staticConverter{}, false
	}
	src, dst, ok := useConverterTypes(file, call, info)
	if !ok {
		return staticConverter{}, false
	}
	fn, ok := converterFunctionName(fset, call.Args[0])
	if !ok {
		return staticConverter{}, false
	}
	return staticConverter{
		srcType: src,
		dstType: dst,
		fn:      fn,
	}, true
}

func mergeConverters(base, override map[string]staticConverter) map[string]staticConverter {
	if len(base) == 0 {
		return override
	}
	if len(override) == 0 {
		return base
	}
	merged := make(map[string]staticConverter, len(base)+len(override))
	for key, converter := range base {
		merged[key] = converter
	}
	for key, converter := range override {
		merged[key] = converter
	}
	return merged
}

func useConverterTypes(file *ast.File, call *ast.CallExpr, info *types.Info) (types.Type, types.Type, bool) {
	var fun ast.Expr
	var typeArgs []ast.Expr
	switch indexed := call.Fun.(type) {
	case *ast.IndexListExpr:
		fun = indexed.X
		typeArgs = indexed.Indices
	case *ast.IndexExpr:
		fun = indexed.X
		typeArgs = []ast.Expr{indexed.Index}
	default:
		return nil, nil, false
	}
	if len(typeArgs) != 2 {
		return nil, nil, false
	}
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "UseConverter" {
		return nil, nil, false
	}
	callLike := &ast.CallExpr{Fun: sel}
	if !isCopierSelectorCall(file, callLike, info) {
		return nil, nil, false
	}
	src := typeExprType(info, typeArgs[0])
	dst := typeExprType(info, typeArgs[1])
	if src == nil || dst == nil {
		return nil, nil, false
	}
	return src, dst, true
}

func typeExprType(info *types.Info, expr ast.Expr) types.Type {
	if info == nil {
		return nil
	}
	if tv, ok := info.Types[expr]; ok && tv.Type != nil && !isInvalidType(tv.Type) {
		return tv.Type
	}
	if t := info.TypeOf(expr); t != nil && !isInvalidType(t) {
		return t
	}
	return nil
}

func converterFunctionName(fset *token.FileSet, expr ast.Expr) (string, bool) {
	switch expr.(type) {
	case *ast.Ident, *ast.SelectorExpr:
	default:
		return "", false
	}
	var b bytes.Buffer
	if err := printer.Fprint(&b, fset, expr); err != nil {
		return "", false
	}
	return b.String(), true
}

func typeKeyOrUnknown(t types.Type) string {
	if t == nil {
		return "<unknown>"
	}
	return typeKey(t)
}

func copyExpressionType(info *types.Info, file *ast.File, pkg *types.Package, explicitTypes map[string]types.Type, expr ast.Expr) types.Type {
	if info == nil {
		return nil
	}
	if t := info.TypeOf(expr); t != nil && !isInvalidType(t) {
		return t
	}
	switch e := expr.(type) {
	case *ast.Ident:
		return explicitTypes[e.Name]
	case *ast.UnaryExpr:
		if e.Op != token.AND {
			return nil
		}
		inner := copyExpressionType(info, file, pkg, explicitTypes, e.X)
		if inner == nil {
			return nil
		}
		return types.NewPointer(inner)
	case *ast.SelectorExpr:
		return selectorExpressionType(info, file, pkg, explicitTypes, e)
	}
	return nil
}

func selectorExpressionType(info *types.Info, file *ast.File, pkg *types.Package, explicitTypes map[string]types.Type, expr *ast.SelectorExpr) types.Type {
	receiver := copyExpressionType(info, file, pkg, explicitTypes, expr.X)
	if receiver == nil {
		return nil
	}
	for {
		ptr, ok := receiver.(*types.Pointer)
		if !ok {
			break
		}
		receiver = ptr.Elem()
	}
	named, ok := receiver.(*types.Named)
	if !ok {
		return nil
	}
	st, ok := named.Underlying().(*types.Struct)
	if !ok {
		return nil
	}
	for i := 0; i < st.NumFields(); i++ {
		field := st.Field(i)
		if field.Name() == expr.Sel.Name {
			return field.Type()
		}
	}
	return nil
}

func collectExplicitTypes(file *ast.File, pkg *types.Package) map[string]types.Type {
	explicitTypes := map[string]types.Type{}
	ast.Inspect(file, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.GenDecl:
			if n.Tok != token.VAR {
				return true
			}
			for _, spec := range n.Specs {
				valueSpec, ok := spec.(*ast.ValueSpec)
				if !ok || valueSpec.Type == nil {
					continue
				}
				t := resolveTypeExpr(file, pkg, valueSpec.Type)
				if t == nil {
					continue
				}
				for _, name := range valueSpec.Names {
					explicitTypes[name.Name] = t
				}
			}
		case *ast.FuncDecl:
			collectFieldListTypes(file, pkg, explicitTypes, n.Type.Params)
			if n.Type.Results != nil {
				collectFieldListTypes(file, pkg, explicitTypes, n.Type.Results)
			}
		}
		return true
	})
	return explicitTypes
}

func collectFieldListTypes(file *ast.File, pkg *types.Package, explicitTypes map[string]types.Type, fields *ast.FieldList) {
	if fields == nil {
		return
	}
	for _, field := range fields.List {
		t := resolveTypeExpr(file, pkg, field.Type)
		if t == nil {
			continue
		}
		for _, name := range field.Names {
			explicitTypes[name.Name] = t
		}
	}
}

func resolveTypeExpr(file *ast.File, pkg *types.Package, expr ast.Expr) types.Type {
	switch e := expr.(type) {
	case *ast.StarExpr:
		inner := resolveTypeExpr(file, pkg, e.X)
		if inner == nil {
			return nil
		}
		return types.NewPointer(inner)
	case *ast.Ident:
		if obj := pkg.Scope().Lookup(e.Name); obj != nil {
			return obj.Type()
		}
	case *ast.SelectorExpr:
		alias, ok := e.X.(*ast.Ident)
		if !ok {
			return nil
		}
		importPath := importPathForAlias(file, alias.Name)
		if importPath == "" {
			return nil
		}
		for _, imported := range pkg.Imports() {
			if imported.Path() != importPath {
				continue
			}
			obj := imported.Scope().Lookup(e.Sel.Name)
			if obj != nil {
				return obj.Type()
			}
		}
	}
	return nil
}

func importPathForAlias(file *ast.File, alias string) string {
	for _, imp := range file.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		name := ""
		if imp.Name != nil {
			name = imp.Name.Name
		} else {
			parts := strings.Split(path, "/")
			name = parts[len(parts)-1]
		}
		if name == alias {
			return path
		}
	}
	return ""
}

func copyDestinationType(info *types.Info, expr ast.Expr) types.Type {
	if info == nil {
		return nil
	}
	if t := info.TypeOf(expr); t != nil {
		return t
	}
	unary, ok := expr.(*ast.UnaryExpr)
	if !ok || unary.Op != token.AND {
		return nil
	}
	inner := info.TypeOf(unary.X)
	if isInvalidType(inner) {
		inner = nil
	}
	if inner == nil {
		return nil
	}
	return types.NewPointer(inner)
}

func isInvalidType(t types.Type) bool {
	if t == nil {
		return false
	}
	return types.TypeString(t, func(*types.Package) string { return "" }) == "invalid type"
}

func isCopierCall(file *ast.File, call *ast.CallExpr, info *types.Info) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if sel.Sel.Name != "Copy" && sel.Sel.Name != "CopyWithOption" {
		return false
	}
	return isCopierSelectorCall(file, call, info)
}

func isCopierSelectorCall(file *ast.File, call *ast.CallExpr, info *types.Info) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	if info != nil {
		if pkgName, ok := info.Uses[ident].(*types.PkgName); ok {
			return isCopierImportPath(pkgName.Imported().Path())
		}
	}
	return copierImportAliases(file)[ident.Name]
}

func copierImportAliases(file *ast.File) map[string]bool {
	aliases := map[string]bool{}
	for _, imp := range file.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		if !isCopierImportPath(path) {
			continue
		}
		switch {
		case imp.Name != nil:
			aliases[imp.Name.Name] = true
		default:
			aliases["copier"] = true
		}
	}
	return aliases
}

func isCopierImportPath(path string) bool {
	return path == "github.com/Alex41/copier-gen"
}

func namedStructFromCopyArg(t types.Type) (*types.Named, bool, bool) {
	if ptr, ok := t.(*types.Pointer); ok {
		if named, ok := ptr.Elem().(*types.Named); ok {
			if _, ok := named.Underlying().(*types.Struct); ok {
				return named, true, true
			}
		}
	}
	if named, ok := t.(*types.Named); ok {
		if _, ok := named.Underlying().(*types.Struct); ok {
			return named, false, true
		}
	}
	return nil, false, false
}

func buildPair(pkg *types.Package, srcName, dstName string, converters map[string]staticConverter) (copyPair, error) {
	srcNamed, srcStruct, err := lookupStruct(pkg, srcName)
	if err != nil {
		return copyPair{}, err
	}
	dstNamed, dstStruct, err := lookupStruct(pkg, dstName)
	if err != nil {
		return copyPair{}, err
	}
	return buildPairFromTypes(copyPair{
		srcName: srcName,
		dstName: dstName,
		srcType: srcNamed,
		dstType: dstNamed,
		src:     srcStruct,
		dst:     dstStruct,
	}, pkg.Path(), converters)
}

func buildPairFromTypes(pair copyPair, currentPkg string, converters map[string]staticConverter) (copyPair, error) {
	srcTags, err := structTags(pair.src)
	if err != nil {
		return copyPair{}, err
	}
	dstTags, err := structTags(pair.dst)
	if err != nil {
		return copyPair{}, err
	}

	for i := 0; i < pair.dst.NumFields(); i++ {
		dstField := pair.dst.Field(i)
		if !dstField.Exported() {
			continue
		}
		dstTag := dstTags[dstField.Name()]
		flags := dstTag.flags
		if flags&tagIgnore != 0 {
			continue
		}

		srcFieldName := sourceNameFor(dstField.Name(), dstTag.name, srcTags)
		srcField, insensitive, ok := findSourceField(pair.src, srcFieldName, srcTags)
		if !ok {
			if flags&tagMust == 0 {
				continue
			}
			return copyPair{}, fmt.Errorf("cannot generate mapper %s -> %s: destination field %s has no source field %s",
				typeKey(pair.srcType), typeKey(pair.dstType), dstField.Name(), srcFieldName)
		}

		assign, ok := assignmentExpr(srcField.Type(), dstField.Type(), "from."+srcField.Name(), currentPkg)
		if !ok {
			nested, nestedOK := nestedFieldCopies(srcField.Type(), dstField.Type(), "from."+srcField.Name(), dstField.Name(), currentPkg)
			if !nestedOK {
				converter, ok := converters[converterKey(srcField.Type(), dstField.Type())]
				if !ok {
					return copyPair{}, fmt.Errorf("cannot generate mapper %s -> %s: field %s needs converter %s -> %s",
						typeKey(pair.srcType), typeKey(pair.dstType), dstField.Name(), typeKey(srcField.Type()), typeKey(dstField.Type()))
				}
				pair.fields = append(pair.fields, fieldCopy{
					srcName:     srcField.Name(),
					srcExpr:     "from." + srcField.Name(),
					dstName:     dstField.Name(),
					srcType:     srcField.Type(),
					dstType:     dstField.Type(),
					flags:       flags,
					zeroExpr:    zeroExpr(srcField.Type(), "from."+srcField.Name()),
					converter:   true,
					converterFn: converter.fn,
					insensitive: insensitive,
					importTypes: []types.Type{srcField.Type(), dstField.Type()},
				})
				continue
			}
			pair.fields = append(pair.fields, fieldCopy{
				srcName:     srcField.Name(),
				srcExpr:     "from." + srcField.Name(),
				dstName:     dstField.Name(),
				srcType:     srcField.Type(),
				dstType:     dstField.Type(),
				flags:       flags,
				zeroExpr:    zeroExpr(srcField.Type(), "from."+srcField.Name()),
				nilSafe:     true,
				zeroAssign:  fmt.Sprintf("copier.Zero[%s]()", typeString(dstField.Type(), currentPkg)),
				nested:      nested,
				importTypes: []types.Type{dstField.Type()},
			})
			continue
		}

		pair.fields = append(pair.fields, fieldCopy{
			srcName:     srcField.Name(),
			srcExpr:     "from." + srcField.Name(),
			dstName:     dstField.Name(),
			srcType:     srcField.Type(),
			dstType:     dstField.Type(),
			flags:       flags,
			zeroExpr:    zeroExpr(srcField.Type(), "from."+srcField.Name()),
			assignExpr:  assign.expr,
			nilSafe:     assign.nilSafe,
			zeroAssign:  assign.zeroAssign,
			insensitive: insensitive,
			importTypes: []types.Type{srcField.Type(), dstField.Type()},
		})
	}

	return pair, nil
}

func nestedFieldCopies(src, dst types.Type, srcExpr, dstPrefix, currentPkg string) ([]fieldCopy, bool) {
	ptr, ok := src.(*types.Pointer)
	if !ok {
		return nil, false
	}
	srcStruct, ok := structType(ptr.Elem())
	if !ok {
		return nil, false
	}
	dstStruct, ok := structType(dst)
	if !ok {
		return nil, false
	}

	var fields []fieldCopy
	for i := 0; i < dstStruct.NumFields(); i++ {
		dstField := dstStruct.Field(i)
		if !dstField.Exported() {
			continue
		}
		srcField, _, ok := findField(srcStruct, dstField.Name())
		if !ok {
			continue
		}
		source := srcExpr + "." + srcField.Name()
		assign, ok := assignmentExpr(srcField.Type(), dstField.Type(), source, currentPkg)
		if !ok {
			return nil, false
		}
		fields = append(fields, fieldCopy{
			srcName:     srcField.Name(),
			srcExpr:     source,
			dstName:     dstPrefix + "." + dstField.Name(),
			srcType:     srcField.Type(),
			dstType:     dstField.Type(),
			flags:       0,
			zeroExpr:    zeroExpr(srcField.Type(), source),
			assignExpr:  assign.expr,
			nilSafe:     assign.nilSafe,
			zeroAssign:  assign.zeroAssign,
			importTypes: []types.Type{srcField.Type(), dstField.Type()},
		})
	}
	return fields, len(fields) > 0
}

func structType(t types.Type) (*types.Struct, bool) {
	if named, ok := t.(*types.Named); ok {
		t = named.Underlying()
	}
	st, ok := t.(*types.Struct)
	return st, ok
}

func lookupStruct(pkg *types.Package, name string) (*types.Named, *types.Struct, error) {
	obj := pkg.Scope().Lookup(name)
	if obj == nil {
		return nil, nil, fmt.Errorf("type %s not found", name)
	}
	named, ok := obj.Type().(*types.Named)
	if !ok {
		return nil, nil, fmt.Errorf("%s is not a named type", name)
	}
	st, ok := named.Underlying().(*types.Struct)
	if !ok {
		return nil, nil, fmt.Errorf("%s is not a struct", name)
	}
	return named, st, nil
}

type tagInfo struct {
	flags uint8
	name  string
}

func structTags(st *types.Struct) (map[string]tagInfo, error) {
	result := map[string]tagInfo{}
	for i := 0; i < st.NumFields(); i++ {
		field := st.Field(i)
		if !field.Exported() {
			continue
		}
		tag := reflect.StructTag(st.Tag(i)).Get("copier")
		info, err := parseTag(tag)
		if err != nil {
			return nil, fmt.Errorf("field %s: %w", field.Name(), err)
		}
		result[field.Name()] = info
	}
	return result, nil
}

func parseTag(tag string) (tagInfo, error) {
	var info tagInfo
	if tag == "" {
		return info, nil
	}
	for _, part := range strings.Split(tag, ",") {
		part = strings.TrimSpace(part)
		switch part {
		case "":
			continue
		case "-":
			info.flags = tagIgnore
			return info, nil
		case "must":
			info.flags |= tagMust
		case "override":
			info.flags |= tagOverride
		default:
			r := []rune(part)
			if len(r) == 0 || !unicode.IsUpper(r[0]) {
				return info, fmt.Errorf("copier field name tag must start uppercase: %q", tag)
			}
			info.name = part
		}
	}
	return info, nil
}

func sourceNameFor(dstFieldName, dstTagName string, srcTags map[string]tagInfo) string {
	if dstTagName != "" {
		for srcField, tag := range srcTags {
			if tag.flags&tagIgnore != 0 {
				continue
			}
			if tag.name == dstTagName {
				return srcField
			}
		}
		return dstTagName
	}
	for srcField, tag := range srcTags {
		if tag.flags&tagIgnore != 0 {
			continue
		}
		if tag.name == dstFieldName {
			return srcField
		}
	}
	return dstFieldName
}

func findSourceField(st *types.Struct, name string, tags map[string]tagInfo) (*types.Var, bool, bool) {
	for i := 0; i < st.NumFields(); i++ {
		field := st.Field(i)
		if !field.Exported() || tags[field.Name()].flags&tagIgnore != 0 {
			continue
		}
		if field.Name() == name {
			return field, false, true
		}
	}
	for i := 0; i < st.NumFields(); i++ {
		field := st.Field(i)
		if !field.Exported() || tags[field.Name()].flags&tagIgnore != 0 {
			continue
		}
		if strings.EqualFold(field.Name(), name) {
			return field, true, true
		}
	}
	return nil, false, false
}

func findField(st *types.Struct, name string) (*types.Var, bool, bool) {
	for i := 0; i < st.NumFields(); i++ {
		field := st.Field(i)
		if !field.Exported() {
			continue
		}
		if field.Name() == name {
			return field, false, true
		}
	}
	for i := 0; i < st.NumFields(); i++ {
		field := st.Field(i)
		if !field.Exported() {
			continue
		}
		if strings.EqualFold(field.Name(), name) {
			return field, true, true
		}
	}
	return nil, false, false
}

type assignment struct {
	expr       string
	nilSafe    bool
	zeroAssign string
}

func assignmentExpr(src, dst types.Type, srcExpr string, currentPkg string) (assignment, bool) {
	if types.AssignableTo(src, dst) {
		return assignment{expr: srcExpr}, true
	}
	if types.ConvertibleTo(src, dst) {
		return assignment{expr: fmt.Sprintf("%s(%s)", typeString(dst, currentPkg), srcExpr)}, true
	}
	if ptr, ok := src.(*types.Pointer); ok {
		elem := ptr.Elem()
		deref := "*" + srcExpr
		if types.AssignableTo(elem, dst) {
			return assignment{
				expr:       deref,
				nilSafe:    true,
				zeroAssign: fmt.Sprintf("copier.Zero[%s]()", typeString(dst, currentPkg)),
			}, true
		}
		if types.ConvertibleTo(elem, dst) {
			return assignment{
				expr:       fmt.Sprintf("%s(%s)", typeString(dst, currentPkg), deref),
				nilSafe:    true,
				zeroAssign: fmt.Sprintf("copier.Zero[%s]()", typeString(dst, currentPkg)),
			}, true
		}
	}
	return assignment{}, false
}

func zeroExpr(t types.Type, expr string) string {
	if isNilable(t) {
		return expr + " == nil"
	}
	if types.Comparable(t) {
		return fmt.Sprintf("copier.IsZero(%s)", expr)
	}
	return "false"
}

func isNilable(t types.Type) bool {
	switch u := t.Underlying().(type) {
	case *types.Pointer, *types.Slice, *types.Map, *types.Interface, *types.Signature:
		return true
	case *types.Named:
		return isNilable(u)
	}
	return false
}

func render(m model) ([]byte, error) {
	var b bytes.Buffer
	imports := collectImports(m)
	fmt.Fprintf(&b, "package %s\n\n", m.pkgName)
	fmt.Fprintf(&b, "// Code generated by copier-gen; DO NOT EDIT.\n\n")
	renderImports(&b, imports)
	for _, pair := range m.pairs {
		renderPair(&b, pair, m.pkgPath)
	}

	formatted, err := format.Source(b.Bytes())
	if err != nil {
		return nil, fmt.Errorf("format generated source: %w\n%s", err, b.String())
	}
	return formatted, nil
}

func writeGeneratedFiles(dir, out string, m model) error {
	if out != "" {
		src, err := render(m)
		if err != nil {
			return err
		}
		outPath := out
		if !filepath.IsAbs(outPath) {
			outPath = filepath.Join(dir, outPath)
		}
		return os.WriteFile(outPath, src, 0644)
	}

	for output, grouped := range groupModelByOutput(m) {
		src, err := render(grouped)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, output), src, 0644); err != nil {
			return err
		}
	}
	return nil
}

func groupModelByOutput(m model) map[string]model {
	grouped := map[string]model{}
	for _, pair := range m.pairs {
		output := generatedFileName(pair.sourceFile)
		next := grouped[output]
		if next.pkgName == "" {
			next = model{pkgName: m.pkgName, pkgPath: m.pkgPath, imports: map[string]string{}}
		}
		next.pairs = append(next.pairs, pair)
		grouped[output] = next
	}
	return grouped
}

func generatedFileName(sourceFile string) string {
	if sourceFile == "" {
		return "copier_gen.go"
	}
	ext := filepath.Ext(sourceFile)
	base := strings.TrimSuffix(sourceFile, ext)
	return base + "_copier_gen.go"
}

func collectImports(m model) map[string]string {
	imports := map[string]string{"github.com/Alex41/copier-gen": "copier"}
	for _, pair := range m.pairs {
		collectTypeImports(imports, pair.srcType, m.pkgPath)
		collectTypeImports(imports, pair.dstType, m.pkgPath)
		for _, field := range pair.fields {
			collectFieldImports(imports, field, m.pkgPath)
		}
	}
	return imports
}

func collectFieldImports(imports map[string]string, field fieldCopy, currentPkg string) {
	for _, t := range field.importTypes {
		collectTypeImports(imports, t, currentPkg)
	}
	for _, nested := range field.nested {
		collectFieldImports(imports, nested, currentPkg)
	}
}

func collectTypeImports(imports map[string]string, t types.Type, currentPkg string) {
	types.TypeString(t, func(pkg *types.Package) string {
		if pkg == nil || pkg.Path() == currentPkg {
			return ""
		}
		if _, ok := imports[pkg.Path()]; !ok {
			imports[pkg.Path()] = uniqueImportName(imports, pkg.Name())
		}
		return imports[pkg.Path()]
	})
}

func renderImports(b *bytes.Buffer, imports map[string]string) {
	paths := make([]string, 0, len(imports))
	for path := range imports {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	fmt.Fprintln(b, "import (")
	for _, path := range paths {
		fmt.Fprintf(b, "%s %q\n", imports[path], path)
	}
	fmt.Fprintln(b, ")")
	fmt.Fprintln(b)
}

func uniqueImportName(imports map[string]string, base string) string {
	name := sanitizeIdentifier(base)
	used := map[string]bool{}
	for _, existing := range imports {
		used[existing] = true
	}
	if !used[name] {
		return name
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s%d", name, i)
		if !used[candidate] {
			return candidate
		}
	}
}

func renderPair(b *bytes.Buffer, pair copyPair, currentPkg string) {
	sort.Slice(pair.fields, func(i, j int) bool {
		return pair.fields[i].dstName < pair.fields[j].dstName
	})

	fn := mapperName(pair)
	dstType := typeString(pair.dstType, currentPkg)
	srcType := typeString(pair.srcType, currentPkg)
	fromType := srcType
	if pair.fromPtr {
		fromType = "*" + srcType
	}
	if pair.discoveredAt != "" {
		fmt.Fprintf(b, "// %s was discovered at %s.\n", fn, pair.discoveredAt)
	}
	fmt.Fprintf(b, "func %s(to *%s, from %s, opt copier.Option) error {\n", fn, dstType, fromType)
	fmt.Fprintln(b, "if to == nil { return copier.ErrInvalidCopyDestination }")
	if pair.fromPtr {
		fmt.Fprintln(b, "if from == nil { return copier.ErrInvalidCopyFrom }")
	}
	for _, field := range pair.fields {
		renderFieldCopy(b, field, currentPkg)
	}
	fmt.Fprintln(b, "return nil")
	fmt.Fprintln(b, "}")
	fmt.Fprintln(b)

	fmt.Fprintf(b, "func init() {\n")
	fmt.Fprintf(b, "copier.RegisterMapper(func(toValue interface{}, fromValue interface{}, opt copier.Option) (bool, error) {\n")
	fmt.Fprintf(b, "to, ok := toValue.(*%s)\n", dstType)
	fmt.Fprintf(b, "if !ok { return false, nil }\n")
	if pair.fromPtr {
		fmt.Fprintf(b, "from, ok := fromValue.(*%s)\n", srcType)
	} else {
		fmt.Fprintf(b, "from, ok := fromValue.(%s)\n", srcType)
	}
	fmt.Fprintf(b, "if !ok { return false, nil }\n")
	fmt.Fprintf(b, "return true, %s(to, from, opt)\n", fn)
	fmt.Fprintf(b, "})\n")
	fmt.Fprintf(b, "}\n")
	fmt.Fprintln(b)

	fmt.Fprintf(b, "func New%sConverter() copier.Converter[%s, %s] {\n", fn, srcType, dstType)
	fmt.Fprintf(b, "return func(src %s) (%s, error) {\n", srcType, dstType)
	fmt.Fprintf(b, "var dst %s\n", dstType)
	fmt.Fprintf(b, "err := %s(&dst, src, copier.Option{})\n", fn)
	fmt.Fprintln(b, "return dst, err")
	fmt.Fprintln(b, "}")
	fmt.Fprintln(b, "}")
	fmt.Fprintln(b)
}

func renderFieldCopy(b *bytes.Buffer, field fieldCopy, currentPkg string) {
	if field.insensitive {
		fmt.Fprintln(b, "if !opt.CaseSensitive {")
	}
	if field.converter {
		fmt.Fprintf(b, "if !copier.ShouldIgnoreEmpty(%s, %d, opt) {\n", field.zeroExpr, field.flags)
		fmt.Fprintf(b, "converted, err := %s(%s)\n", field.converterFn, field.srcExpr)
		fmt.Fprintln(b, "if err != nil { return err }")
		fmt.Fprintf(b, "to.%s = converted\n", field.dstName)
		fmt.Fprintln(b, "}")
	} else if len(field.nested) > 0 {
		fmt.Fprintf(b, "if %s != nil {\n", field.srcExpr)
		for _, nested := range field.nested {
			renderFieldCopy(b, nested, currentPkg)
		}
		fmt.Fprintf(b, "} else if !copier.ShouldIgnoreEmpty(true, %d, opt) {\n", field.flags)
		fmt.Fprintf(b, "to.%s = %s\n", field.dstName, field.zeroAssign)
		fmt.Fprintln(b, "}")
	} else if field.nilSafe {
		fmt.Fprintf(b, "if %s != nil {\n", field.srcExpr)
		fmt.Fprintf(b, "to.%s = %s\n", field.dstName, field.assignExpr)
		fmt.Fprintf(b, "} else if !copier.ShouldIgnoreEmpty(true, %d, opt) {\n", field.flags)
		fmt.Fprintf(b, "to.%s = %s\n", field.dstName, field.zeroAssign)
		fmt.Fprintln(b, "}")
	} else {
		fmt.Fprintf(b, "if !copier.ShouldIgnoreEmpty(%s, %d, opt) {\n", field.zeroExpr, field.flags)
		fmt.Fprintf(b, "to.%s = %s\n", field.dstName, field.assignExpr)
		fmt.Fprintln(b, "}")
	}
	if field.insensitive {
		fmt.Fprintln(b, "}")
	}
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

func typeString(t types.Type, currentPkg string) string {
	return types.TypeString(t, func(pkg *types.Package) string {
		if pkg == nil {
			return ""
		}
		if pkg.Path() == currentPkg {
			return ""
		}
		return sanitizeIdentifier(pkg.Name())
	})
}

func typeKey(t types.Type) string {
	return types.TypeString(t, func(pkg *types.Package) string {
		if pkg == nil {
			return ""
		}
		return pkg.Path()
	})
}

func converterKey(src, dst types.Type) string {
	return typeKey(src) + "->" + typeKey(dst)
}

func mapperName(pair copyPair) string {
	return "Copy" + typeNameForFunc(pair.srcType) + "To" + typeNameForFunc(pair.dstType)
}

func typeNameForFunc(t types.Type) string {
	named, ok := t.(*types.Named)
	if !ok {
		return sanitizeIdentifier(types.TypeString(t, func(pkg *types.Package) string {
			if pkg == nil {
				return ""
			}
			return pkg.Name()
		}))
	}
	prefix := ""
	if pkg := named.Obj().Pkg(); pkg != nil {
		prefix = sanitizeIdentifier(pkg.Name())
	}
	return upperFirst(prefix) + upperFirst(sanitizeIdentifier(named.Obj().Name()))
}

var nonIdentifier = regexp.MustCompile(`[^a-zA-Z0-9_]`)

func sanitizeIdentifier(s string) string {
	s = nonIdentifier.ReplaceAllString(s, "_")
	if s == "" {
		return "x"
	}
	if unicode.IsDigit([]rune(s)[0]) {
		return "_" + s
	}
	return s
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func fatalf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "copier-gen: "+format+"\n", args...)
	os.Exit(1)
}
