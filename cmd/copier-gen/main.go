package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/importer"
	"go/parser"
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
	pkgName string
	pkgPath string
	pairs   []copyPair
	imports map[string]string
	issues  []string
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
	discoveredAt string
}

type fieldCopy struct {
	srcName     string
	dstName     string
	flags       uint8
	zeroExpr    string
	assignExpr  string
	insensitive bool
}

const (
	tagIgnore uint8 = 1 << iota
	tagOverride
	tagMust
)

func main() {
	var pairs pairFlag
	out := flag.String("out", "copier_gen.go", "generated file path")
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

	src, err := render(m)
	if err != nil {
		fatalf("%v", err)
	}

	outPath := *out
	if !filepath.IsAbs(outPath) {
		outPath = filepath.Join(*dir, outPath)
	}
	if err := os.WriteFile(outPath, src, 0644); err != nil {
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
	m := model{pkgName: pkg.Name, pkgPath: pkg.PkgPath, imports: map[string]string{}}
	for _, err := range pkg.Errors {
		m.issues = append(m.issues, err.Error())
	}
	seen := map[string]bool{}
	discovered, issues := discoverCopyPairs(pkg.Fset, pkg.Syntax, pkg.TypesInfo, pkg.Types)
	m.issues = append(m.issues, issues...)
	for _, pair := range discovered {
		key := typeKey(pair.srcType) + "->" + typeKey(pair.dstType)
		if seen[key] {
			continue
		}
		seen[key] = true
		built, err := buildPairFromTypes(pair, pkg.PkgPath)
		if err != nil {
			return model{}, err
		}
		m.pairs = append(m.pairs, built)
	}
	for _, raw := range rawPairs {
		parts := strings.SplitN(raw, ":", 2)
		pair, err := buildPair(pkg.Types, strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]))
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

	m := model{pkgName: pkg.Name(), pkgPath: pkg.Path(), imports: map[string]string{}}
	seen := map[string]bool{}
	discovered, issues := discoverCopyPairs(fset, files, info, pkg)
	m.issues = append(m.issues, issues...)
	for _, pair := range discovered {
		key := typeKey(pair.srcType) + "->" + typeKey(pair.dstType)
		if seen[key] {
			continue
		}
		seen[key] = true
		built, err := buildPairFromTypes(pair, pkg.Path())
		if err != nil {
			return model{}, err
		}
		m.pairs = append(m.pairs, built)
	}

	for _, raw := range rawPairs {
		parts := strings.SplitN(raw, ":", 2)
		pair, err := buildPair(pkg, strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]))
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
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) < 2 {
				return true
			}
			if !isCopierCall(file, call, info) {
				return true
			}
			pos := fset.Position(call.Lparen)
			location := fmt.Sprintf("%s:%d", filepath.Base(pos.Filename), pos.Line)
			dstType := info.TypeOf(call.Args[0])
			srcType := info.TypeOf(call.Args[1])
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
			pairs = append(pairs, copyPair{
				srcName:      srcNamed.Obj().Name(),
				dstName:      dstNamed.Obj().Name(),
				srcType:      srcNamed,
				dstType:      dstNamed,
				src:          srcNamed.Underlying().(*types.Struct),
				dst:          dstNamed.Underlying().(*types.Struct),
				fromPtr:      fromPtr,
				discoveredAt: fmt.Sprintf("%s:%d", filepath.Base(pos.Filename), pos.Line),
			})
			_ = pkg
			return true
		})
	}
	return pairs, issues
}

func typeKeyOrUnknown(t types.Type) string {
	if t == nil {
		return "<unknown>"
	}
	return typeKey(t)
}

func isCopierCall(file *ast.File, call *ast.CallExpr, info *types.Info) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if sel.Sel.Name != "Copy" && sel.Sel.Name != "CopyWithOption" {
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

func buildPair(pkg *types.Package, srcName, dstName string) (copyPair, error) {
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
	}, pkg.Path())
}

func buildPairFromTypes(pair copyPair, currentPkg string) (copyPair, error) {
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
		srcField, insensitive, ok := findField(pair.src, srcFieldName)
		if !ok {
			if flags&tagMust == 0 {
				continue
			}
			return copyPair{}, fmt.Errorf("cannot generate mapper %s -> %s: destination field %s has no source field %s",
				typeKey(pair.srcType), typeKey(pair.dstType), dstField.Name(), srcFieldName)
		}

		assign, ok := assignmentExpr(srcField.Type(), dstField.Type(), "from."+srcField.Name(), currentPkg)
		if !ok {
			return copyPair{}, fmt.Errorf("cannot generate mapper %s -> %s: field %s cannot assign %s to %s",
				typeKey(pair.srcType), typeKey(pair.dstType), dstField.Name(), typeKey(srcField.Type()), typeKey(dstField.Type()))
		}

		pair.fields = append(pair.fields, fieldCopy{
			srcName:     srcField.Name(),
			dstName:     dstField.Name(),
			flags:       flags,
			zeroExpr:    zeroExpr(srcField.Type(), "from."+srcField.Name()),
			assignExpr:  assign,
			insensitive: insensitive,
		})
	}

	return pair, nil
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
			if tag.name == dstTagName {
				return srcField
			}
		}
		return dstTagName
	}
	for srcField, tag := range srcTags {
		if tag.name == dstFieldName {
			return srcField
		}
	}
	return dstFieldName
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

func assignmentExpr(src, dst types.Type, srcExpr string, currentPkg string) (string, bool) {
	if types.AssignableTo(src, dst) {
		return srcExpr, true
	}
	if types.ConvertibleTo(src, dst) {
		return fmt.Sprintf("%s(%s)", typeString(dst, currentPkg), srcExpr), true
	}
	return "", false
}

func zeroExpr(t types.Type, expr string) string {
	if isNilable(t) {
		return expr + " == nil"
	}
	if types.Comparable(t) {
		return fmt.Sprintf("copierGenIsZero(%s)", expr)
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
	fmt.Fprintln(&b, "func copierGenIsZero[T comparable](v T) bool { var zero T; return v == zero }")
	fmt.Fprintln(&b)

	for _, pair := range m.pairs {
		renderPair(&b, pair, m.pkgPath)
	}

	formatted, err := format.Source(b.Bytes())
	if err != nil {
		return nil, fmt.Errorf("format generated source: %w\n%s", err, b.String())
	}
	return formatted, nil
}

func collectImports(m model) map[string]string {
	imports := map[string]string{"github.com/Alex41/copier-gen": "copier"}
	for _, pair := range m.pairs {
		collectTypeImports(imports, pair.srcType, m.pkgPath)
		collectTypeImports(imports, pair.dstType, m.pkgPath)
		for _, field := range pair.fields {
			_ = field
		}
	}
	return imports
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
		if field.insensitive {
			fmt.Fprintln(b, "if !opt.CaseSensitive {")
		}
		fmt.Fprintf(b, "if !copier.ShouldIgnoreEmpty(%s, %d, opt) {\n", field.zeroExpr, field.flags)
		fmt.Fprintf(b, "to.%s = %s\n", field.dstName, field.assignExpr)
		fmt.Fprintln(b, "}")
		if field.insensitive {
			fmt.Fprintln(b, "}")
		}
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
