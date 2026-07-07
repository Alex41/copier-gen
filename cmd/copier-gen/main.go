package main

import (
	"bytes"
	"crypto/sha256"
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
	nested     []nestedMapper
	nestedSet  bool
	imports    map[string]string
	issues     []string
	warnings   []string
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
	toIndirect   bool
	deepCopy     bool
	fields       []fieldCopy
	converters   map[string]staticConverter
	warnings     []string
	sourceFile   string
	discoveredAt string
}

type nestedMapper struct {
	name    string
	srcType types.Type
	dstType types.Type
	fields  []fieldCopy
}

type fieldCopy struct {
	srcName          string
	srcExpr          string
	dstName          string
	srcType          types.Type
	dstType          types.Type
	flags            uint8
	zeroExpr         string
	assignExpr       string
	nilSafe          bool
	zeroAssign       string
	nested           []fieldCopy
	nestedPtr        bool
	nestedAlloc      string
	nestedSrcType    types.Type
	nestedDstType    types.Type
	ptrCopy          bool
	sliceCopy        bool
	sliceNested      bool
	converter        bool
	slice            bool
	converterPtr     bool
	converterArgPtr  bool
	converterItemPtr bool
	converterFn      string
	converterSrcType types.Type
	converterDstType types.Type
	converterContext bool
	insensitive      bool
	importTypes      []types.Type
}

type sourceField struct {
	field *types.Var
	expr  string
}

type staticConverter struct {
	srcType     types.Type
	dstType     types.Type
	fn          string
	withContext bool
}

const (
	tagIgnore uint8 = 1 << iota
	tagOverride
	tagMust
	tagInitSlice
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
	if len(m.issues) > 0 {
		fatalf("found copier calls, but could not generate mappers:\n%s", strings.Join(m.issues, "\n"))
	}
	for _, warning := range m.warnings {
		fmt.Fprintf(os.Stderr, "copier-gen: warning: %s\n", warning)
	}
	if len(m.pairs) == 0 {
		fatalf("no copier.Copy calls found; pass -pair Src:Dst for an explicit mapper")
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
	overlay, err := generatedFileOverlay(dir)
	if err != nil {
		return model{}, err
	}
	cfg := &packages.Config{
		Mode: packages.NeedName |
			packages.NeedFiles |
			packages.NeedSyntax |
			packages.NeedTypes |
			packages.NeedTypesInfo |
			packages.NeedImports,
		Dir:     dir,
		Tests:   false,
		Overlay: overlay,
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
	if len(issues) > 0 {
		return model{}, generationError{err: copyDiscoveryError(issues)}
	}
	for _, pair := range discovered {
		key := copyPairKey(pair)
		if seen[key] {
			continue
		}
		seen[key] = true
		built, err := buildPairFromTypes(pair, pkg.PkgPath, mergeConverters(m.converters, pair.converters))
		if err != nil {
			return model{}, generationError{err: pairBuildError(pair, err)}
		}
		m.warnings = append(m.warnings, built.warnings...)
		m.warnings = append(m.warnings, unusedConverterWarnings(pair, built)...)
		m.pairs = append(m.pairs, built)
	}
	for _, raw := range rawPairs {
		parts := strings.SplitN(raw, ":", 2)
		pair, err := buildPair(pkg.Types, strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), m.converters)
		if err != nil {
			return model{}, err
		}
		key := copyPairKey(pair)
		if !seen[key] {
			seen[key] = true
			m.pairs = append(m.pairs, pair)
		}
	}
	return m, nil
}

func generatedFileOverlay(dir string) (map[string][]byte, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	pkgName := ""
	var generated []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		if isGeneratedGoFile(name) {
			generated = append(generated, path)
			continue
		}
		if pkgName != "" {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.PackageClauseOnly)
		if err != nil {
			return nil, err
		}
		pkgName = file.Name.Name
	}
	if pkgName == "" || len(generated) == 0 {
		return nil, nil
	}
	overlay := make(map[string][]byte, len(generated))
	for _, path := range generated {
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		overlay[abs] = []byte("package " + pkgName + "\n")
	}
	return overlay, nil
}

func isGeneratedGoFile(name string) bool {
	return strings.HasSuffix(name, "_gen.go")
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
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || isGeneratedGoFile(name) {
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
	if len(issues) > 0 {
		return model{}, copyDiscoveryError(issues)
	}
	for _, pair := range discovered {
		key := copyPairKey(pair)
		if seen[key] {
			continue
		}
		seen[key] = true
		built, err := buildPairFromTypes(pair, pkg.Path(), mergeConverters(m.converters, pair.converters))
		if err != nil {
			return model{}, pairBuildError(pair, err)
		}
		m.warnings = append(m.warnings, built.warnings...)
		m.warnings = append(m.warnings, unusedConverterWarnings(pair, built)...)
		m.pairs = append(m.pairs, built)
	}

	for _, raw := range rawPairs {
		parts := strings.SplitN(raw, ":", 2)
		pair, err := buildPair(pkg, strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), m.converters)
		if err != nil {
			return model{}, err
		}
		key := copyPairKey(pair)
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
			discoveredAt := positionString(pos)
			sourceFile := filepath.Base(pos.Filename)
			location := discoveredAt
			dstType := copyExpressionType(info, file, pkg, explicitTypes, call.Args[0])
			srcType := copyExpressionType(info, file, pkg, explicitTypes, call.Args[1])
			dstNamed, toIndirect, ok := namedStructDestination(dstType)
			if !ok {
				issues = append(issues, fmt.Sprintf("%s: destination argument must be a writable pointer to a named struct: %s", location, typeKeyOrUnknown(dstType)))
				return true
			}
			srcNamed, fromPtr, ok := namedStructFromCopyArg(srcType)
			if !ok {
				issues = append(issues, fmt.Sprintf("%s: source argument is not a named struct or pointer to named struct: %s", location, typeKeyOrUnknown(srcType)))
				return true
			}
			var option copierOption
			if len(call.Args) >= 3 {
				option = discoverOption(fset, file, info, call.Args[2])
			}
			pairs = append(pairs, copyPair{
				srcName:      srcNamed.Obj().Name(),
				dstName:      dstNamed.Obj().Name(),
				srcType:      srcNamed,
				dstType:      dstNamed,
				src:          srcNamed.Underlying().(*types.Struct),
				dst:          dstNamed.Underlying().(*types.Struct),
				fromPtr:      fromPtr,
				toIndirect:   toIndirect,
				deepCopy:     option.deepCopy,
				converters:   option.converters,
				sourceFile:   sourceFile,
				discoveredAt: discoveredAt,
			})
			_ = pkg
			return true
		})
	}
	return pairs, issues
}

func positionString(pos token.Position) string {
	filename := pos.Filename
	if filename != "" && !filepath.IsAbs(filename) {
		if abs, err := filepath.Abs(filename); err == nil {
			filename = abs
		}
	}
	if filename == "" {
		filename = "-"
	}
	return fmt.Sprintf("%s:%d", filename, pos.Line)
}

func pairBuildError(pair copyPair, err error) error {
	if pair.discoveredAt == "" {
		return err
	}
	return fmt.Errorf("%s: %w", pair.discoveredAt, err)
}

func copyDiscoveryError(issues []string) error {
	return fmt.Errorf("found copier calls that cannot be generated:\n%s", strings.Join(issues, "\n"))
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

type copierOption struct {
	deepCopy   bool
	converters map[string]staticConverter
}

func discoverOption(fset *token.FileSet, file *ast.File, info *types.Info, expr ast.Expr) copierOption {
	return copierOption{
		deepCopy:   discoverOptionDeepCopy(expr),
		converters: discoverOptionConverters(fset, file, info, expr),
	}
}

func discoverOptionDeepCopy(expr ast.Expr) bool {
	lit, ok := expr.(*ast.CompositeLit)
	if !ok {
		return false
	}
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || key.Name != "DeepCopy" {
			continue
		}
		value, ok := kv.Value.(*ast.Ident)
		return ok && value.Name == "true"
	}
	return false
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
	src, dst, withContext, ok := useConverterTypes(file, call, info)
	if !ok {
		return staticConverter{}, false
	}
	fn, ok := converterFunctionName(fset, call.Args[0])
	if !ok {
		return staticConverter{}, false
	}
	return staticConverter{
		srcType:     src,
		dstType:     dst,
		fn:          fn,
		withContext: withContext,
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

func useConverterTypes(file *ast.File, call *ast.CallExpr, info *types.Info) (types.Type, types.Type, bool, bool) {
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
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !isUseConverterName(sel.Sel.Name) {
			return nil, nil, false, false
		}
		if !isCopierSelectorCall(file, call, info) {
			return nil, nil, false, false
		}
		src, dst, ok := converterArgTypes(info, call.Args[0], sel.Sel.Name == "UseConverterContext")
		return src, dst, sel.Sel.Name == "UseConverterContext", ok
	}
	if len(typeArgs) != 2 {
		return nil, nil, false, false
	}
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok || !isUseConverterName(sel.Sel.Name) {
		return nil, nil, false, false
	}
	callLike := &ast.CallExpr{Fun: sel}
	if !isCopierSelectorCall(file, callLike, info) {
		return nil, nil, false, false
	}
	src := typeExprType(info, typeArgs[0])
	dst := typeExprType(info, typeArgs[1])
	if src == nil || dst == nil {
		return nil, nil, false, false
	}
	return src, dst, sel.Sel.Name == "UseConverterContext", true
}

func isUseConverterName(name string) bool {
	return name == "UseConverter" || name == "UseConverterContext"
}

func converterArgTypes(info *types.Info, expr ast.Expr, withContext bool) (types.Type, types.Type, bool) {
	if info == nil {
		return nil, nil, false
	}
	t := info.TypeOf(expr)
	if t == nil || isInvalidType(t) {
		return nil, nil, false
	}
	sig, ok := t.Underlying().(*types.Signature)
	if !ok || sig.Params().Len() != 1 || sig.Results().Len() != 2 {
		if !withContext || !ok || sig.Params().Len() != 2 || sig.Results().Len() != 2 {
			return nil, nil, false
		}
		if !isContextType(sig.Params().At(0).Type()) {
			return nil, nil, false
		}
	} else if withContext {
		return nil, nil, false
	}
	errType := sig.Results().At(1).Type()
	if errType == nil || errType.String() != "error" {
		return nil, nil, false
	}
	if withContext {
		return sig.Params().At(1).Type(), sig.Results().At(0).Type(), true
	}
	return sig.Params().At(0).Type(), sig.Results().At(0).Type(), true
}

func isContextType(t types.Type) bool {
	named, ok := t.(*types.Named)
	if !ok {
		return false
	}
	obj := named.Obj()
	return obj.Name() == "Context" && obj.Pkg() != nil && obj.Pkg().Path() == "context"
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
	if sel.Sel.Name != "Copy" {
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

func namedStructDestination(t types.Type) (*types.Named, bool, bool) {
	ptr, ok := t.(*types.Pointer)
	if !ok {
		return nil, false, false
	}
	elem := ptr.Elem()
	indirect := false
	if nested, ok := elem.(*types.Pointer); ok {
		indirect = true
		elem = nested.Elem()
	}
	named, ok := elem.(*types.Named)
	if !ok {
		return nil, false, false
	}
	if _, ok := named.Underlying().(*types.Struct); !ok {
		return nil, false, false
	}
	return named, indirect, true
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
		if flags&tagInitSlice != 0 && !isSliceType(dstField.Type()) {
			return copyPair{}, fmt.Errorf("cannot generate mapper %s -> %s: destination field %s uses init_slice but is not a slice",
				typeKey(pair.srcType), typeKey(pair.dstType), dstField.Name())
		}

		srcFieldName := sourceNameFor(dstField.Name(), dstTag.name, srcTags)
		srcField, insensitive, ok := findSourceField(pair.src, srcFieldName, srcTags)
		if !ok {
			if flags&tagMust == 0 {
				pair.warnings = append(pair.warnings, skippedDestinationFieldWarning(
					pair.discoveredAt, pair.srcType, pair.dstType, dstField.Name(), srcFieldName,
				))
				continue
			}
			return copyPair{}, fmt.Errorf("cannot generate mapper %s -> %s: destination field %s has no source field %s",
				typeKey(pair.srcType), typeKey(pair.dstType), dstField.Name(), srcFieldName)
		}

		if converter, ok := converters[converterKey(srcField.field.Type(), dstField.Type())]; ok {
			pair.fields = append(pair.fields, converterFieldCopy(srcField, dstField, flags, insensitive, converter))
			continue
		}
		if converter, ok := pointerDestinationConverter(srcField.field.Type(), dstField.Type(), converters); ok {
			pair.fields = append(pair.fields, pointerConverterFieldCopy(srcField, dstField, flags, insensitive, converter))
			continue
		}

		if pair.deepCopy && needsDeepCopy(srcField.field.Type()) {
			if converter, argPtr, itemPtr, ok := sliceElementConverter(srcField.field.Type(), dstField.Type(), converters); ok {
				pair.fields = append(pair.fields, sliceConverterFieldCopy(srcField, dstField, flags, insensitive, converter, argPtr, itemPtr))
				continue
			}
			if field, ok := deepFieldCopy(srcField, dstField, flags, insensitive, currentPkg, converters, &pair.warnings, pair); ok {
				pair.fields = append(pair.fields, field)
				continue
			}
			if assign, ok := assignmentExpr(srcField.field.Type(), dstField.Type(), "from."+srcField.expr, currentPkg); ok && assign.nilSafe {
				pair.fields = append(pair.fields, assignmentFieldCopy(srcField, dstField, flags, insensitive, assign))
				continue
			}
			return copyPair{}, fmt.Errorf("cannot generate deep copy mapper %s -> %s: field %s needs supported deep copy or converter %s -> %s",
				typeKey(pair.srcType), typeKey(pair.dstType), dstField.Name(), typeKey(srcField.field.Type()), typeKey(dstField.Type()))
		}

		assign, ok := assignmentExpr(srcField.field.Type(), dstField.Type(), "from."+srcField.expr, currentPkg)
		if !ok {
			if converter, argPtr, itemPtr, ok := sliceElementConverter(srcField.field.Type(), dstField.Type(), converters); ok {
				pair.fields = append(pair.fields, sliceConverterFieldCopy(srcField, dstField, flags, insensitive, converter, argPtr, itemPtr))
				continue
			}
			nested, nestedPtr, nestedAlloc, nestedOK := nestedFieldCopies(
				srcField.field.Type(), dstField.Type(), "from."+srcField.expr, dstField.Name(), currentPkg, converters, &pair.warnings, pair,
			)
			if !nestedOK {
				{
					return copyPair{}, fmt.Errorf("cannot generate mapper %s -> %s: field %s needs converter %s -> %s",
						typeKey(pair.srcType), typeKey(pair.dstType), dstField.Name(), typeKey(srcField.field.Type()), typeKey(dstField.Type()))
				}
			}
			pair.fields = append(pair.fields, nestedCopyField(
				srcField, dstField, flags, insensitive, nested, nestedPtr, nestedAlloc, currentPkg,
			))
			continue
		}

		pair.fields = append(pair.fields, assignmentFieldCopy(srcField, dstField, flags, insensitive, assign))
	}

	return pair, nil
}

func converterFieldCopy(srcField sourceField, dstField *types.Var, flags uint8, insensitive bool, converter staticConverter) fieldCopy {
	field := baseFieldCopy(srcField, dstField, flags, insensitive)
	return applyConverter(field, converter)
}

func applyConverter(field fieldCopy, converter staticConverter) fieldCopy {
	field.converter = true
	field.converterFn = converter.fn
	field.converterSrcType = converter.srcType
	field.converterDstType = converter.dstType
	field.converterContext = converter.withContext
	field.importTypes = []types.Type{converter.srcType, converter.dstType}
	return field
}

func sliceConverterFieldCopy(srcField sourceField, dstField *types.Var, flags uint8, insensitive bool, converter staticConverter, argPtr, itemPtr bool) fieldCopy {
	field := converterFieldCopy(srcField, dstField, flags, insensitive, converter)
	field.slice = true
	field.converterArgPtr = argPtr
	field.converterItemPtr = itemPtr
	field.importTypes = append(field.importTypes, dstField.Type())
	return field
}

func sliceConverterFieldCopyValues(
	srcName string,
	srcExpr string,
	srcType types.Type,
	dstField *types.Var,
	flags uint8,
	insensitive bool,
	converter staticConverter,
	argPtr bool,
	itemPtr bool,
) fieldCopy {
	field := baseFieldCopyValues(srcName, srcExpr, srcType, dstField, flags, insensitive)
	field = applyConverter(field, converter)
	field.slice = true
	field.converterArgPtr = argPtr
	field.converterItemPtr = itemPtr
	field.importTypes = append(field.importTypes, dstField.Type())
	return field
}

func pointerConverterFieldCopy(srcField sourceField, dstField *types.Var, flags uint8, insensitive bool, converter staticConverter) fieldCopy {
	field := converterFieldCopy(srcField, dstField, flags, insensitive, converter)
	field.converterPtr = true
	return field
}

func baseFieldCopy(srcField sourceField, dstField *types.Var, flags uint8, insensitive bool) fieldCopy {
	srcExpr := "from." + srcField.expr
	return baseFieldCopyValues(srcField.field.Name(), srcExpr, srcField.field.Type(), dstField, flags, insensitive)
}

func baseFieldCopyValues(
	srcName string,
	srcExpr string,
	srcType types.Type,
	dstField *types.Var,
	flags uint8,
	insensitive bool,
) fieldCopy {
	return fieldCopy{
		srcName:     srcName,
		srcExpr:     srcExpr,
		dstName:     dstField.Name(),
		srcType:     srcType,
		dstType:     dstField.Type(),
		flags:       flags,
		zeroExpr:    zeroExpr(srcType, srcExpr),
		insensitive: insensitive,
	}
}

func assignmentFieldCopy(srcField sourceField, dstField *types.Var, flags uint8, insensitive bool, assign assignment) fieldCopy {
	field := baseFieldCopy(srcField, dstField, flags, insensitive)
	field.assignExpr = assign.expr
	field.nilSafe = assign.nilSafe
	field.zeroAssign = assign.zeroAssign
	field.importTypes = assign.importTypes
	return field
}

func assignmentFieldCopyValues(
	srcName string,
	srcExpr string,
	srcType types.Type,
	dstField *types.Var,
	assign assignment,
) fieldCopy {
	field := baseFieldCopyValues(srcName, srcExpr, srcType, dstField, 0, false)
	field.assignExpr = assign.expr
	field.nilSafe = assign.nilSafe
	field.zeroAssign = assign.zeroAssign
	field.importTypes = assign.importTypes
	return field
}

func nestedCopyField(
	srcField sourceField,
	dstField *types.Var,
	flags uint8,
	insensitive bool,
	nested []fieldCopy,
	nestedPtr bool,
	nestedAlloc string,
	currentPkg string,
) fieldCopy {
	field := baseFieldCopy(srcField, dstField, flags, insensitive)
	field.nilSafe = true
	field.zeroAssign = fmt.Sprintf("copiergen.Zero[%s]()", typeString(dstField.Type(), currentPkg))
	field.nested = nested
	field.nestedPtr = nestedPtr
	field.nestedAlloc = nestedAlloc
	field.importTypes = []types.Type{dstField.Type()}
	return field
}

func deepFieldCopy(
	srcField sourceField,
	dstField *types.Var,
	flags uint8,
	insensitive bool,
	currentPkg string,
	converters map[string]staticConverter,
	warnings *[]string,
	pair copyPair,
) (fieldCopy, bool) {
	srcExpr := "from." + srcField.expr
	if nested, nestedPtr, nestedAlloc, ok := nestedFieldCopies(
		srcField.field.Type(), dstField.Type(), srcExpr, dstField.Name(), currentPkg, converters, warnings, pair,
	); ok {
		return nestedCopyField(srcField, dstField, flags, insensitive, nested, nestedPtr, nestedAlloc, currentPkg), true
	}
	if assign, ok := pointerDeepAssignment(srcField.field.Type(), dstField.Type(), srcExpr, currentPkg); ok {
		field := assignmentFieldCopy(srcField, dstField, flags, insensitive, assign)
		field.ptrCopy = true
		field.zeroAssign = fmt.Sprintf("copiergen.Zero[%s]()", typeString(dstField.Type(), currentPkg))
		field.importTypes = append(field.importTypes, dstField.Type())
		return field, true
	}
	if assign, ok := sliceDeepAssignment(srcField.field.Type(), dstField.Type(), currentPkg); ok {
		field := assignmentFieldCopy(srcField, dstField, flags, insensitive, assign)
		field.sliceCopy = true
		field.zeroAssign = fmt.Sprintf("copiergen.Zero[%s]()", typeString(dstField.Type(), currentPkg))
		field.importTypes = append(field.importTypes, dstField.Type())
		return field, true
	}
	if field, ok := sliceNestedFieldCopy(srcField, dstField, flags, insensitive, currentPkg, converters, warnings, pair); ok {
		return field, true
	}
	return fieldCopy{}, false
}

func sliceNestedFieldCopy(
	srcField sourceField,
	dstField *types.Var,
	flags uint8,
	insensitive bool,
	currentPkg string,
	converters map[string]staticConverter,
	warnings *[]string,
	pair copyPair,
) (fieldCopy, bool) {
	return sliceNestedFieldCopyValues(
		srcField.field.Name(), "from."+srcField.expr, srcField.field.Type(), dstField, flags, insensitive, currentPkg, converters, warnings, pair,
	)
}

func sliceNestedFieldCopyValues(
	srcName string,
	srcExpr string,
	srcType types.Type,
	dstField *types.Var,
	flags uint8,
	insensitive bool,
	currentPkg string,
	converters map[string]staticConverter,
	warnings *[]string,
	pair copyPair,
) (fieldCopy, bool) {
	srcSlice, ok := srcType.Underlying().(*types.Slice)
	if !ok {
		return fieldCopy{}, false
	}
	dstSlice, ok := dstField.Type().Underlying().(*types.Slice)
	if !ok {
		return fieldCopy{}, false
	}
	srcElem := srcSlice.Elem()
	dstElem := dstSlice.Elem()
	nested, nestedPtr, nestedAlloc, ok := nestedFieldCopies(
		srcElem, dstElem, "item", "convertedItem", currentPkg, converters, warnings, pair,
	)
	if !ok {
		return fieldCopy{}, false
	}
	field := baseFieldCopyValues(srcName, srcExpr, srcType, dstField, flags, insensitive)
	field.sliceNested = true
	field.nilSafe = true
	field.zeroAssign = fmt.Sprintf("copiergen.Zero[%s]()", typeString(dstField.Type(), currentPkg))
	field.nested = nested
	field.nestedPtr = nestedPtr
	field.nestedAlloc = nestedAlloc
	field.nestedSrcType = srcElem
	field.nestedDstType = dstElem
	field.importTypes = []types.Type{dstField.Type(), dstElem}
	return field, true
}

func pointerDeepAssignment(src, dst types.Type, srcExpr string, currentPkg string) (assignment, bool) {
	srcPtr, ok := src.(*types.Pointer)
	if !ok {
		return assignment{}, false
	}
	dstPtr, ok := dst.(*types.Pointer)
	if !ok {
		return assignment{}, false
	}
	if !isTimeType(srcPtr.Elem()) {
		if _, ok := structType(srcPtr.Elem()); ok {
			return assignment{}, false
		}
	}
	if !isTimeType(dstPtr.Elem()) {
		if _, ok := structType(dstPtr.Elem()); ok {
			return assignment{}, false
		}
	}
	if isTimeType(srcPtr.Elem()) != isTimeType(dstPtr.Elem()) {
		return assignment{}, false
	}
	assign, ok := assignmentExpr(srcPtr.Elem(), dstPtr.Elem(), "*"+srcExpr, currentPkg)
	return assign, ok
}

func isTimeType(t types.Type) bool {
	named, ok := t.(*types.Named)
	if !ok {
		return false
	}
	obj := named.Obj()
	return obj.Name() == "Time" && obj.Pkg() != nil && obj.Pkg().Path() == "time"
}

func sliceDeepAssignment(src, dst types.Type, currentPkg string) (assignment, bool) {
	srcSlice, ok := src.Underlying().(*types.Slice)
	if !ok {
		return assignment{}, false
	}
	dstSlice, ok := dst.Underlying().(*types.Slice)
	if !ok {
		return assignment{}, false
	}
	assign, ok := assignmentExpr(srcSlice.Elem(), dstSlice.Elem(), "item", currentPkg)
	return assign, ok
}

func nestedFieldCopies(
	src, dst types.Type,
	srcExpr, dstPrefix, currentPkg string,
	converters map[string]staticConverter,
	warnings *[]string,
	pair copyPair,
) ([]fieldCopy, bool, string, bool) {
	return nestedFieldCopiesDepth(src, dst, srcExpr, dstPrefix, currentPkg, converters, warnings, pair, 0)
}

func nestedFieldCopiesDepth(
	src, dst types.Type,
	srcExpr, dstPrefix, currentPkg string,
	converters map[string]staticConverter,
	warnings *[]string,
	pair copyPair,
	depth int,
) ([]fieldCopy, bool, string, bool) {
	if depth > 8 {
		return nil, false, "", false
	}
	ptr, ok := src.(*types.Pointer)
	if !ok {
		return nil, false, "", false
	}
	srcStruct, ok := structType(ptr.Elem())
	if !ok {
		return nil, false, "", false
	}
	dstPtr := false
	dstStructType := dst
	if ptr, ok := dst.(*types.Pointer); ok {
		dstPtr = true
		dstStructType = ptr.Elem()
	}
	dstStruct, ok := structType(dstStructType)
	if !ok {
		return nil, false, "", false
	}
	srcTags, err := structTags(srcStruct)
	if err != nil {
		return nil, false, "", false
	}
	dstTags, err := structTags(dstStruct)
	if err != nil {
		return nil, false, "", false
	}

	var fields []fieldCopy
	for i := 0; i < dstStruct.NumFields(); i++ {
		dstField := dstStruct.Field(i)
		if !dstField.Exported() {
			continue
		}
		dstTag := dstTags[dstField.Name()]
		flags := dstTag.flags
		if flags&tagIgnore != 0 {
			continue
		}
		if flags&tagInitSlice != 0 && !isSliceType(dstField.Type()) {
			return nil, false, "", false
		}
		srcFieldName := sourceNameFor(dstField.Name(), dstTag.name, srcTags)
		srcField, insensitive, ok := findSourceField(srcStruct, srcFieldName, srcTags)
		if !ok {
			if flags&tagMust != 0 {
				return nil, false, "", false
			}
			if warnings != nil {
				*warnings = append(*warnings, skippedDestinationFieldWarning(
					pair.discoveredAt, pair.srcType, pair.dstType, dstPrefix+"."+dstField.Name(), srcFieldName,
				))
			}
			continue
		}
		source := srcExpr + "." + srcField.expr
		if converter, ok := converters[converterKey(srcField.field.Type(), dstField.Type())]; ok {
			field := baseFieldCopyValues(srcField.field.Name(), source, srcField.field.Type(), dstField, flags, insensitive)
			field.dstName = dstPrefix + "." + dstField.Name()
			fields = append(fields, applyConverter(field, converter))
			continue
		}
		if converter, ok := pointerDestinationConverter(srcField.field.Type(), dstField.Type(), converters); ok {
			field := baseFieldCopyValues(srcField.field.Name(), source, srcField.field.Type(), dstField, flags, insensitive)
			field.dstName = dstPrefix + "." + dstField.Name()
			field = applyConverter(field, converter)
			field.converterPtr = true
			fields = append(fields, field)
			continue
		}
		assign, ok := assignmentExpr(srcField.field.Type(), dstField.Type(), source, currentPkg)
		if !ok {
			if converter, argPtr, itemPtr, ok := sliceElementConverter(srcField.field.Type(), dstField.Type(), converters); ok {
				field := sliceConverterFieldCopyValues(
					srcField.field.Name(), source, srcField.field.Type(), dstField, flags, insensitive, converter, argPtr, itemPtr,
				)
				field.dstName = dstPrefix + "." + dstField.Name()
				fields = append(fields, field)
				continue
			}
			nested, nestedPtr, nestedAlloc, nestedOK := nestedFieldCopiesDepth(
				srcField.field.Type(), dstField.Type(), source, dstPrefix+"."+dstField.Name(), currentPkg, converters, warnings, pair, depth+1,
			)
			if !nestedOK {
				if field, ok := sliceNestedFieldCopyValues(
					srcField.field.Name(), source, srcField.field.Type(), dstField, flags, insensitive, currentPkg, converters, warnings, pair,
				); ok {
					field.dstName = dstPrefix + "." + dstField.Name()
					fields = append(fields, field)
					continue
				}
				return nil, false, "", false
			}
			field := baseFieldCopyValues(
				srcField.field.Name(), source, srcField.field.Type(), dstField, flags, insensitive,
			)
			field.dstName = dstPrefix + "." + dstField.Name()
			field.nilSafe = true
			field.zeroAssign = fmt.Sprintf("copiergen.Zero[%s]()", typeString(dstField.Type(), currentPkg))
			field.nested = nested
			field.nestedPtr = nestedPtr
			field.nestedAlloc = nestedAlloc
			field.importTypes = []types.Type{dstField.Type()}
			fields = append(fields, field)
			continue
		}
		field := assignmentFieldCopyValues(srcField.field.Name(), source, srcField.field.Type(), dstField, assign)
		field.dstName = dstPrefix + "." + dstField.Name()
		field.flags = flags
		field.insensitive = insensitive
		fields = append(fields, field)
	}
	alloc := ""
	if dstPtr {
		alloc = typeString(dstStructType, currentPkg)
	}
	return fields, dstPtr, alloc, len(fields) > 0
}

func sliceElementConverter(src, dst types.Type, converters map[string]staticConverter) (staticConverter, bool, bool, bool) {
	srcSlice, ok := src.Underlying().(*types.Slice)
	if !ok {
		return staticConverter{}, false, false, false
	}
	dstSlice, ok := dst.Underlying().(*types.Slice)
	if !ok {
		return staticConverter{}, false, false, false
	}
	srcElem := srcSlice.Elem()
	dstElem := dstSlice.Elem()
	if converter, ok := converters[converterKey(srcElem, dstElem)]; ok {
		return converter, false, false, true
	}
	srcElemPtr := types.NewPointer(srcElem)
	if converter, ok := converters[converterKey(srcElemPtr, dstElem)]; ok {
		return converter, true, false, true
	}
	dstElemPtr := types.NewPointer(dstElem)
	if converter, ok := converters[converterKey(srcElemPtr, dstElemPtr)]; ok {
		return converter, true, true, true
	}
	if converter, ok := converters[converterKey(srcElem, dstElemPtr)]; ok {
		return converter, false, true, true
	}
	return staticConverter{}, false, false, false
}

func pointerDestinationConverter(src, dst types.Type, converters map[string]staticConverter) (staticConverter, bool) {
	dstPtr, ok := dst.(*types.Pointer)
	if !ok {
		return staticConverter{}, false
	}
	converter, ok := converters[converterKey(src, dstPtr.Elem())]
	return converter, ok
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
		case "init_slice":
			info.flags |= tagInitSlice
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

func isSliceType(t types.Type) bool {
	_, ok := t.Underlying().(*types.Slice)
	return ok
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

func findSourceField(st *types.Struct, name string, tags map[string]tagInfo) (sourceField, bool, bool) {
	return findSourceFieldWithPrefix(st, name, tags, "")
}

func findSourceFieldWithPrefix(st *types.Struct, name string, tags map[string]tagInfo, prefix string) (sourceField, bool, bool) {
	for i := 0; i < st.NumFields(); i++ {
		field := st.Field(i)
		if !field.Exported() || tags[field.Name()].flags&tagIgnore != 0 {
			continue
		}
		if field.Name() == name {
			return sourceField{field: field, expr: prefix + field.Name()}, false, true
		}
	}
	for i := 0; i < st.NumFields(); i++ {
		field := st.Field(i)
		if !field.Exported() || tags[field.Name()].flags&tagIgnore != 0 {
			continue
		}
		if strings.EqualFold(field.Name(), name) {
			return sourceField{field: field, expr: prefix + field.Name()}, true, true
		}
	}
	for i := 0; i < st.NumFields(); i++ {
		field := st.Field(i)
		if !field.Exported() || !field.Anonymous() || tags[field.Name()].flags&tagIgnore != 0 {
			continue
		}
		embedded, ok := structType(field.Type())
		if !ok {
			continue
		}
		embeddedTags, err := structTags(embedded)
		if err != nil {
			continue
		}
		found, insensitive, ok := findSourceFieldWithPrefix(embedded, name, embeddedTags, prefix+field.Name()+".")
		if ok {
			return found, insensitive, true
		}
	}
	return sourceField{}, false, false
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
	expr        string
	nilSafe     bool
	zeroAssign  string
	importTypes []types.Type
}

func assignmentExpr(src, dst types.Type, srcExpr string, currentPkg string) (assignment, bool) {
	if types.AssignableTo(src, dst) {
		return assignment{expr: srcExpr}, true
	}
	if types.ConvertibleTo(src, dst) {
		return assignment{
			expr:        fmt.Sprintf("%s(%s)", typeString(dst, currentPkg), srcExpr),
			importTypes: []types.Type{dst},
		}, true
	}
	if ptr, ok := src.(*types.Pointer); ok {
		elem := ptr.Elem()
		deref := "*" + srcExpr
		if types.AssignableTo(elem, dst) {
			return assignment{
				expr:        deref,
				nilSafe:     true,
				zeroAssign:  fmt.Sprintf("copiergen.Zero[%s]()", typeString(dst, currentPkg)),
				importTypes: []types.Type{dst},
			}, true
		}
		if types.ConvertibleTo(elem, dst) {
			return assignment{
				expr:        fmt.Sprintf("%s(%s)", typeString(dst, currentPkg), deref),
				nilSafe:     true,
				zeroAssign:  fmt.Sprintf("copiergen.Zero[%s]()", typeString(dst, currentPkg)),
				importTypes: []types.Type{dst},
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
		return fmt.Sprintf("copiergen.IsZero(%s)", expr)
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

func needsDeepCopy(t types.Type) bool {
	switch t.Underlying().(type) {
	case *types.Pointer, *types.Slice, *types.Map:
		return true
	}
	return false
}

func render(m model) ([]byte, error) {
	var b bytes.Buffer
	imports := collectImports(m)
	fmt.Fprintf(&b, "package %s\n\n", m.pkgName)
	fmt.Fprintf(&b, "// Code generated by copier-gen; DO NOT EDIT.\n\n")
	renderImports(&b, imports)
	nested := m.nested
	if !m.nestedSet {
		nested = collectNestedMappers(m)
	}
	for _, mapper := range nested {
		renderNestedMapper(&b, mapper, m.pkgPath)
	}
	for _, pair := range m.pairs {
		renderPair(&b, pair, m.pkgPath)
	}
	renderMapperRegistrations(&b, m.pairs, m.pkgPath)

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

	rendered := make(map[string][]byte)
	for output, grouped := range groupModelByOutput(m) {
		src, err := render(grouped)
		if err != nil {
			return err
		}
		rendered[output] = src
	}
	for output, src := range rendered {
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
	outputs := make([]string, 0, len(grouped))
	for output := range grouped {
		outputs = append(outputs, output)
	}
	sort.Strings(outputs)

	owned := map[string]bool{}
	for _, output := range outputs {
		next := grouped[output]
		next.nestedSet = true
		for _, mapper := range collectNestedMappers(next) {
			key := nestedMapperKey(mapper.srcType, mapper.dstType, mapper.fields)
			if owned[key] {
				continue
			}
			owned[key] = true
			next.nested = append(next.nested, mapper)
		}
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
		if len(pair.fields) > 0 {
			imports["github.com/Alex41/copier-gen/runtime"] = "copiergen"
		}
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
		alias := imports[path]
		if alias == defaultImportName(path) {
			fmt.Fprintf(b, "%q\n", path)
			continue
		}
		fmt.Fprintf(b, "%s %q\n", alias, path)
	}
	fmt.Fprintln(b, ")")
	fmt.Fprintln(b)
}

func defaultImportName(path string) string {
	parts := strings.Split(path, "/")
	return sanitizeIdentifier(parts[len(parts)-1])
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

func collectNestedMappers(m model) []nestedMapper {
	byKey := map[string]nestedMapper{}
	var collect func(field fieldCopy)
	collect = func(field fieldCopy) {
		if len(field.nested) == 0 {
			return
		}
		fields := normalizeNestedFieldsForField(field)
		srcType, dstType := nestedMapperTypesForField(field)
		key := nestedMapperKey(srcType, dstType, fields)
		if _, ok := byKey[key]; !ok {
			byKey[key] = nestedMapper{
				name:    nestedMapperName(srcType, dstType, fields),
				srcType: srcType,
				dstType: dstType,
				fields:  fields,
			}
		}
		for _, child := range field.nested {
			collect(child)
		}
	}
	for _, pair := range m.pairs {
		for _, field := range pair.fields {
			collect(field)
		}
	}

	mappers := make([]nestedMapper, 0, len(byKey))
	for _, mapper := range byKey {
		mappers = append(mappers, mapper)
	}
	sort.Slice(mappers, func(i, j int) bool {
		return mappers[i].name < mappers[j].name
	})
	return mappers
}

func normalizeNestedFields(fields []fieldCopy, srcPrefix, dstPrefix string) []fieldCopy {
	normalized := make([]fieldCopy, 0, len(fields))
	for _, field := range fields {
		next := field
		next.srcExpr = replaceExpressionRoot(field.srcExpr, srcPrefix, "from")
		next.zeroExpr = replaceExpressionRoot(field.zeroExpr, srcPrefix, "from")
		next.assignExpr = replaceExpressionRoot(field.assignExpr, srcPrefix, "from")
		next.dstName = strings.TrimPrefix(field.dstName, dstPrefix+".")
		next.nested = normalizeNestedFields(field.nested, srcPrefix, dstPrefix)
		normalized = append(normalized, next)
	}
	return normalized
}

func replaceExpressionRoot(expr, oldRoot, newRoot string) string {
	if expr == oldRoot {
		return newRoot
	}
	expr = strings.ReplaceAll(expr, oldRoot+".", newRoot+".")
	return strings.ReplaceAll(expr, oldRoot+" ", newRoot+" ")
}

func renderNestedMapper(b *bytes.Buffer, mapper nestedMapper, currentPkg string) {
	dstType := typeString(mapper.dstType, currentPkg)
	toType := dstType
	if _, ok := mapper.dstType.(*types.Pointer); !ok {
		toType = "*" + dstType
	}
	fmt.Fprintf(b, "func %s(to %s, from %s, opt copier.Option) error {\n",
		mapper.name, toType, typeString(mapper.srcType, currentPkg))
	fmt.Fprintln(b, "if to == nil { return copier.ErrInvalidCopyDestination }")
	fmt.Fprintln(b, "if from == nil { return copier.ErrInvalidCopyFrom }")
	for _, field := range mapper.fields {
		renderFieldCopy(b, field, currentPkg)
	}
	fmt.Fprintln(b, "return nil")
	fmt.Fprintln(b, "}")
	fmt.Fprintln(b)
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
	toType := "*" + dstType
	toName := "to"
	if pair.toIndirect {
		toType = "**" + dstType
		toName = "toValue"
	}
	fmt.Fprintf(b, "func %s(%s %s, from %s, opt copier.Option) error {\n", fn, toName, toType, fromType)
	if pair.toIndirect {
		fmt.Fprintln(b, "if toValue == nil || *toValue == nil { return copier.ErrInvalidCopyDestination }")
		fmt.Fprintln(b, "to := *toValue")
	} else {
		fmt.Fprintln(b, "if to == nil { return copier.ErrInvalidCopyDestination }")
	}
	if pair.fromPtr {
		fmt.Fprintln(b, "if from == nil { return copier.ErrInvalidCopyFrom }")
	}
	for _, field := range pair.fields {
		renderFieldCopy(b, field, currentPkg)
	}
	fmt.Fprintln(b, "return nil")
	fmt.Fprintln(b, "}")
	fmt.Fprintln(b)
}

func renderMapperRegistrations(b *bytes.Buffer, pairs []copyPair, currentPkg string) {
	if len(pairs) == 0 {
		return
	}
	fmt.Fprintln(b, "func init() {")
	for _, pair := range pairs {
		renderMapperRegistration(b, pair, currentPkg)
	}
	fmt.Fprintln(b, "}")
	fmt.Fprintln(b)
}

func renderMapperRegistration(b *bytes.Buffer, pair copyPair, currentPkg string) {
	fn := mapperName(pair)
	dstType := typeString(pair.dstType, currentPkg)
	srcType := typeString(pair.srcType, currentPkg)
	fmt.Fprintf(b, "copier.RegisterMapper(func(toValue, fromValue any, opt copier.Option) (bool, error) {\n")
	if pair.toIndirect {
		fmt.Fprintf(b, "to, ok := toValue.(**%s)\n", dstType)
	} else {
		fmt.Fprintf(b, "to, ok := toValue.(*%s)\n", dstType)
	}
	fmt.Fprintf(b, "if !ok { return false, nil }\n")
	if pair.fromPtr {
		fmt.Fprintf(b, "from, ok := fromValue.(*%s)\n", srcType)
	} else {
		fmt.Fprintf(b, "from, ok := fromValue.(%s)\n", srcType)
	}
	fmt.Fprintf(b, "if !ok { return false, nil }\n")
	fmt.Fprintf(b, "return true, %s(to, from, opt)\n", fn)
	fmt.Fprintf(b, "})\n")
}

func renderFieldCopy(b *bytes.Buffer, field fieldCopy, currentPkg string) {
	if field.insensitive {
		fmt.Fprintln(b, "if !opt.CaseSensitive {")
	}
	if field.converter {
		renderCopyBlockStart(b, field)
		if field.converterContext {
			fmt.Fprintf(b, "converter, ok := copier.FindConverterContext[%s, %s](opt.Converters)\n",
				typeString(field.converterSrcType, currentPkg), typeString(field.converterDstType, currentPkg))
		} else {
			fmt.Fprintf(b, "converter, ok := copier.FindConverter[%s, %s](opt.Converters)\n",
				typeString(field.converterSrcType, currentPkg), typeString(field.converterDstType, currentPkg))
		}
		fmt.Fprintln(b, "if !ok { return copier.ErrGeneratedConverterNotFound }")
		if field.slice {
			fmt.Fprintf(b, "var converted %s\n", typeString(field.dstType, currentPkg))
			if fieldInitSlice(field) {
				fmt.Fprintf(b, "converted = %s\n", initSliceExpr(field.dstType, currentPkg))
			}
			fmt.Fprintf(b, "if %s != nil {\n", field.srcExpr)
			fmt.Fprintf(b, "converted = make(%s, 0, len(%s))\n", typeString(field.dstType, currentPkg), field.srcExpr)
			fmt.Fprintf(b, "for _, item := range %s {\n", field.srcExpr)
			argExpr := "item"
			if field.converterArgPtr {
				argExpr = "&item"
			}
			if field.converterContext {
				fmt.Fprintf(b, "convertedItem, err := converter(opt.Context, %s)\n", argExpr)
			} else {
				fmt.Fprintf(b, "convertedItem, err := converter(%s)\n", argExpr)
			}
			fmt.Fprintln(b, "if err != nil { return err }")
			if field.converterItemPtr {
				fmt.Fprintln(b, "if convertedItem == nil {")
				fmt.Fprintf(b, "converted = append(converted, copiergen.Zero[%s]())\n", typeString(field.converterDstType.(*types.Pointer).Elem(), currentPkg))
				fmt.Fprintln(b, "} else {")
				fmt.Fprintln(b, "converted = append(converted, *convertedItem)")
				fmt.Fprintln(b, "}")
			} else {
				fmt.Fprintln(b, "converted = append(converted, convertedItem)")
			}
			fmt.Fprintln(b, "}")
			fmt.Fprintln(b, "}")
		} else {
			if field.converterContext {
				fmt.Fprintf(b, "converted, err := converter(opt.Context, %s)\n", field.srcExpr)
			} else {
				fmt.Fprintf(b, "converted, err := converter(%s)\n", field.srcExpr)
			}
			fmt.Fprintln(b, "if err != nil { return err }")
		}
		if field.converterPtr {
			fmt.Fprintf(b, "to.%s = &converted\n", field.dstName)
		} else {
			fmt.Fprintf(b, "to.%s = converted\n", field.dstName)
		}
		fmt.Fprintln(b, "}")
	} else if field.ptrCopy {
		fmt.Fprintf(b, "if %s != nil {\n", field.srcExpr)
		fmt.Fprintf(b, "copied := %s\n", field.assignExpr)
		fmt.Fprintf(b, "to.%s = &copied\n", field.dstName)
		renderNilSourceElse(b, field)
		fmt.Fprintf(b, "to.%s = %s\n", field.dstName, zeroAssignExpr(field, currentPkg))
		fmt.Fprintln(b, "}")
	} else if field.sliceNested {
		fmt.Fprintf(b, "if %s != nil {\n", field.srcExpr)
		fmt.Fprintf(b, "copied := make(%s, 0, len(%s))\n", typeString(field.dstType, currentPkg), field.srcExpr)
		fmt.Fprintf(b, "for _, item := range %s {\n", field.srcExpr)
		fmt.Fprintln(b, "if item == nil {")
		if field.nestedPtr {
			fmt.Fprintln(b, "copied = append(copied, nil)")
		} else {
			fmt.Fprintf(b, "copied = append(copied, copiergen.Zero[%s]())\n", typeString(field.nestedDstType, currentPkg))
		}
		fmt.Fprintln(b, "continue")
		fmt.Fprintln(b, "}")
		toExpr := "&convertedItem"
		if field.nestedPtr {
			fmt.Fprintf(b, "convertedItem := new(%s)\n", field.nestedAlloc)
			toExpr = "convertedItem"
		} else {
			fmt.Fprintf(b, "var convertedItem %s\n", typeString(field.nestedDstType, currentPkg))
		}
		fmt.Fprintf(b, "if err := %s(%s, item, opt); err != nil { return err }\n", nestedMapperNameForField(field), toExpr)
		fmt.Fprintln(b, "copied = append(copied, convertedItem)")
		fmt.Fprintln(b, "}")
		fmt.Fprintf(b, "to.%s = copied\n", field.dstName)
		renderNilSourceElse(b, field)
		fmt.Fprintf(b, "to.%s = %s\n", field.dstName, zeroAssignExpr(field, currentPkg))
		fmt.Fprintln(b, "}")
	} else if len(field.nested) > 0 {
		fmt.Fprintf(b, "if %s != nil {\n", field.srcExpr)
		toExpr := "&to." + field.dstName
		if field.nestedPtr {
			fmt.Fprintf(b, "to.%s = new(%s)\n", field.dstName, field.nestedAlloc)
			toExpr = "to." + field.dstName
		}
		fmt.Fprintf(b, "if err := %s(%s, %s, opt); err != nil { return err }\n",
			nestedMapperNameForField(field),
			toExpr,
			field.srcExpr,
		)
		renderNilSourceElse(b, field)
		fmt.Fprintf(b, "to.%s = %s\n", field.dstName, zeroAssignExpr(field, currentPkg))
		fmt.Fprintln(b, "}")
	} else if field.sliceCopy {
		fmt.Fprintf(b, "if %s != nil {\n", field.srcExpr)
		fmt.Fprintf(b, "copied := make(%s, 0, len(%s))\n", typeString(field.dstType, currentPkg), field.srcExpr)
		fmt.Fprintf(b, "for _, item := range %s {\n", field.srcExpr)
		fmt.Fprintf(b, "copied = append(copied, %s)\n", field.assignExpr)
		fmt.Fprintln(b, "}")
		fmt.Fprintf(b, "to.%s = copied\n", field.dstName)
		renderNilSourceElse(b, field)
		fmt.Fprintf(b, "to.%s = %s\n", field.dstName, zeroAssignExpr(field, currentPkg))
		fmt.Fprintln(b, "}")
	} else if field.nilSafe {
		fmt.Fprintf(b, "if %s != nil {\n", field.srcExpr)
		fmt.Fprintf(b, "to.%s = %s\n", field.dstName, field.assignExpr)
		renderNilSourceElse(b, field)
		fmt.Fprintf(b, "to.%s = %s\n", field.dstName, zeroAssignExpr(field, currentPkg))
		fmt.Fprintln(b, "}")
	} else if fieldInitSlice(field) {
		fmt.Fprintf(b, "if %s != nil {\n", field.srcExpr)
		fmt.Fprintf(b, "to.%s = %s\n", field.dstName, field.assignExpr)
		fmt.Fprintln(b, "} else {")
		fmt.Fprintf(b, "to.%s = %s\n", field.dstName, initSliceExpr(field.dstType, currentPkg))
		fmt.Fprintln(b, "}")
	} else {
		fmt.Fprintf(b, "if !copiergen.ShouldIgnoreEmpty(%s, %d, opt) {\n", field.zeroExpr, field.flags)
		fmt.Fprintf(b, "to.%s = %s\n", field.dstName, field.assignExpr)
		fmt.Fprintln(b, "}")
	}
	if field.insensitive {
		fmt.Fprintln(b, "}")
	}
}

func renderCopyBlockStart(b *bytes.Buffer, field fieldCopy) {
	if fieldInitSlice(field) {
		fmt.Fprintln(b, "{")
		return
	}
	fmt.Fprintf(b, "if !copiergen.ShouldIgnoreEmpty(%s, %d, opt) {\n", field.zeroExpr, field.flags)
}

func renderNilSourceElse(b *bytes.Buffer, field fieldCopy) {
	if fieldInitSlice(field) {
		fmt.Fprintln(b, "} else {")
		return
	}
	fmt.Fprintf(b, "} else if !copiergen.ShouldIgnoreEmpty(true, %d, opt) {\n", field.flags)
}

func zeroAssignExpr(field fieldCopy, currentPkg string) string {
	if fieldInitSlice(field) {
		return initSliceExpr(field.dstType, currentPkg)
	}
	return field.zeroAssign
}

func initSliceExpr(t types.Type, currentPkg string) string {
	return fmt.Sprintf("make(%s, 0)", typeString(t, currentPkg))
}

func fieldInitSlice(field fieldCopy) bool {
	return field.flags&tagInitSlice != 0
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

func skippedDestinationFieldWarning(location string, srcType, dstType types.Type, dstField, srcField string) string {
	if location == "" {
		location = "-"
	}
	return fmt.Sprintf(
		"%s: destination field %s is not written in mapper %s -> %s: source field %s was not found",
		location,
		dstField,
		typeKey(srcType),
		typeKey(dstType),
		srcField,
	)
}

func unusedConverterWarnings(discovered, built copyPair) []string {
	if len(discovered.converters) == 0 {
		return nil
	}
	used := map[string]bool{}
	collectUsedConverterKeys(used, built.fields)
	keys := make([]string, 0, len(discovered.converters))
	for key := range discovered.converters {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var warnings []string
	for _, key := range keys {
		if used[key] {
			continue
		}
		converter := discovered.converters[key]
		location := discovered.discoveredAt
		if location == "" {
			location = "-"
		}
		warnings = append(warnings, fmt.Sprintf(
			"%s: converter %s for %s -> %s was provided but not used",
			location,
			converter.fn,
			typeKey(converter.srcType),
			typeKey(converter.dstType),
		))
	}
	return warnings
}

func collectUsedConverterKeys(used map[string]bool, fields []fieldCopy) {
	for _, field := range fields {
		if field.converter {
			used[converterKey(field.converterSrcType, field.converterDstType)] = true
		}
		collectUsedConverterKeys(used, field.nested)
	}
}

func copyPairKey(pair copyPair) string {
	return fmt.Sprintf(
		"%t:%s->%t:%s",
		pair.fromPtr,
		typeKey(pair.srcType),
		pair.toIndirect,
		typeKey(pair.dstType),
	)
}

func mapperName(pair copyPair) string {
	hash := sha256.Sum256([]byte("mapper:" + copyPairKey(pair)))
	return fmt.Sprintf("_copier_%x", hash[:8])
}

func nestedMapperKey(src, dst types.Type, fields []fieldCopy) string {
	var plan strings.Builder
	writeFieldPlan(&plan, fields)
	return typeKey(src) + "->" + typeKey(dst) + ":" + plan.String()
}

func writeFieldPlan(plan *strings.Builder, fields []fieldCopy) {
	for _, field := range fields {
		fmt.Fprintf(
			plan,
			"%s|%s|%s|%s|%d|%t|%t|%t|%t|%t|%t|%t|%t|%t|%t|%s|%s|%s|%s|",
			field.srcExpr,
			field.dstName,
			typeKey(field.srcType),
			typeKey(field.dstType),
			field.flags,
			field.nilSafe,
			field.ptrCopy,
			field.sliceCopy,
			field.sliceNested,
			field.converter,
			field.slice,
			field.converterPtr,
			field.converterArgPtr,
			field.converterItemPtr,
			field.converterContext,
			field.assignExpr,
			field.zeroAssign,
			typeKeyOrUnknown(field.nestedSrcType),
			typeKeyOrUnknown(field.nestedDstType),
		)
		if field.converter {
			fmt.Fprintf(plan, "%s|%s|", typeKey(field.converterSrcType), typeKey(field.converterDstType))
		}
		plan.WriteByte('{')
		writeFieldPlan(plan, field.nested)
		plan.WriteString("};")
	}
}

func nestedMapperName(src, dst types.Type, fields []fieldCopy) string {
	hash := sha256.Sum256([]byte("nested:" + nestedMapperKey(src, dst, fields)))
	return fmt.Sprintf("_copier_%x", hash[:8])
}

func nestedMapperNameForField(field fieldCopy) string {
	fields := normalizeNestedFieldsForField(field)
	srcType, dstType := nestedMapperTypesForField(field)
	return nestedMapperName(srcType, dstType, fields)
}

func normalizeNestedFieldsForField(field fieldCopy) []fieldCopy {
	if field.sliceNested {
		return normalizeNestedFields(field.nested, "item", "convertedItem")
	}
	return normalizeNestedFields(field.nested, field.srcExpr, field.dstName)
}

func nestedMapperTypesForField(field fieldCopy) (types.Type, types.Type) {
	if field.sliceNested {
		return field.nestedSrcType, field.nestedDstType
	}
	return field.srcType, field.dstType
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

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "copier-gen: "+format+"\n", args...)
	os.Exit(1)
}
