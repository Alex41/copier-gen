package main

import (
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestRenderStructPairWithTags(t *testing.T) {
	dir := t.TempDir()
	src := `package sample

import copier "github.com/Alex41/copier-gen"

type User struct {
	Name string
	Age int ` + "`copier:\"Years\"`" + `
	Skip string
	alias string
	Title string
	Required string
}

type Employee struct {
	FullName string ` + "`copier:\"Name\"`" + `
	Years int
	Secret string ` + "`copier:\"-\"`" + `
	Required string ` + "`copier:\"must\"`" + `
	title string
	TITLE string
}

func Cast(src User) (Employee, error) {
	var dst Employee
	err := copier.Copy(&dst, src, copier.Option{IgnoreEmpty: true})
	return dst, err
}
`
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	m, err := loadModel(dir, nil)
	if err != nil {
		t.Fatalf("loadModel returned error: %v", err)
	}
	out, err := render(m)
	if err != nil {
		t.Fatalf("render returned error: %v", err)
	}
	text := string(out)
	mapper := mapperName(m.pairs[0])
	for _, want := range []string{
		`copiergen "github.com/Alex41/copier-gen/runtime"`,
		"func " + mapper + "(to *Employee, from User, opt copier.Option) error",
		"to.FullName = from.Name",
		"to.Years = from.Age",
		"if !opt.CaseSensitive",
		"to.TITLE = from.Title",
		"to.Required = from.Required",
		"copier.RegisterMapper(func(toValue, fromValue any, opt copier.Option) (bool, error)",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("generated output does not contain %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "func copySampleUserToSampleEmployee") ||
		strings.Contains(text, "func CopySampleUserToSampleEmployee") ||
		strings.Contains(text, "func New") {
		t.Fatalf("generated output contains exported helper functions:\n%s", text)
	}
	if strings.Contains(text, "to.Secret") {
		t.Fatalf("ignored field was generated:\n%s", text)
	}
	if strings.Contains(text, "CheckMust") {
		t.Fatalf("runtime must check was generated:\n%s", text)
	}
}

func TestRenderDoesNotImportDirectAssignmentFieldTypes(t *testing.T) {
	dir := t.TempDir()
	src := `package sample

import (
	"time"

	copier "github.com/Alex41/copier-gen"
)

type User struct {
	CreatedAt time.Time
}

type Employee struct {
	CreatedAt time.Time
}

func Cast(src User) (Employee, error) {
	var dst Employee
	err := copier.Copy(&dst, src, copier.Option{})
	return dst, err
}
`
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	m, err := loadModel(dir, nil)
	if err != nil {
		t.Fatalf("loadModel returned error: %v", err)
	}
	out, err := render(m)
	if err != nil {
		t.Fatalf("render returned error: %v", err)
	}
	text := string(out)
	if strings.Contains(text, `"time"`) || strings.Contains(text, `time "time"`) {
		t.Fatalf("generated output imports unused time package:\n%s", text)
	}
	if !strings.Contains(text, "to.CreatedAt = from.CreatedAt") {
		t.Fatalf("generated output does not copy time field:\n%s", text)
	}
}

func TestLoadModelFailsWhenMapperCannotBeGenerated(t *testing.T) {
	dir := t.TempDir()
	src := `package sample

import copier "github.com/Alex41/copier-gen"

type User struct {
	Name string
}

type Employee struct {
	Name string
	Required string ` + "`copier:\"must\"`" + `
}

func Cast(src User) (Employee, error) {
	var dst Employee
	err := copier.Copy(&dst, src, copier.Option{})
	return dst, err
}
`
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := loadModel(dir, nil)
	if err == nil {
		t.Fatal("loadModel succeeded for an incomplete mapper")
	}
	if !strings.Contains(err.Error(), "Required") {
		t.Fatalf("error does not name the missing field: %v", err)
	}
	if !strings.Contains(err.Error(), filepath.Join(dir, "sample.go")+":16") {
		t.Fatalf("error does not include copier call location: %v", err)
	}
}

func TestLoadModelIgnoresStaleGeneratedFiles(t *testing.T) {
	dir := t.TempDir()
	src := `package sample

import copier "github.com/Alex41/copier-gen"

type Source struct {
	Name string
}

type Destination struct {
	Name string
}

func Cast(src Source) (Destination, error) {
	var dst Destination
	err := copier.Copy(&dst, src, copier.Option{})
	return dst, err
}
`
	staleGenerated := `package sample

func stale(from *Source) {
	_ = from.Image
}
`
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sample_copier_gen.go"), []byte(staleGenerated), 0644); err != nil {
		t.Fatal(err)
	}

	m, err := loadModel(dir, nil)
	if err != nil {
		t.Fatalf("loadModel returned error for stale generated file: %v", err)
	}
	if len(m.pairs) != 1 {
		t.Fatalf("expected one discovered pair, got %d", len(m.pairs))
	}
}

func TestLoadModelDetectsCopierAlias(t *testing.T) {
	dir := t.TempDir()
	src := `package sample

import cg "github.com/Alex41/copier-gen"

type User struct {
	Name string
}

type Employee struct {
	Name string
}

func Cast(src User) (Employee, error) {
	var dst Employee
	err := cg.Copy(&dst, src, cg.Option{})
	return dst, err
}
`
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	m, err := loadModel(dir, nil)
	if err != nil {
		t.Fatalf("loadModel returned error: %v", err)
	}
	if len(m.pairs) != 1 {
		t.Fatalf("expected one discovered pair, got %d", len(m.pairs))
	}
}

func TestLoadModelRejectsNoPanicTag(t *testing.T) {
	dir := t.TempDir()
	src := `package sample

import copier "github.com/Alex41/copier-gen"

type User struct {
	Name string
}

type Employee struct {
	Name string ` + "`copier:\"must,nopanic\"`" + `
}

func Cast(src User) (Employee, error) {
	var dst Employee
	err := copier.Copy(&dst, src, copier.Option{})
	return dst, err
}
`
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := loadModel(dir, nil)
	if err == nil {
		t.Fatal("loadModel accepted nopanic tag")
	}
	if !strings.Contains(err.Error(), "nopanic") {
		t.Fatalf("error does not name unsupported tag: %v", err)
	}
}

func TestLoadModelDetectsAddressedSelectorDestination(t *testing.T) {
	dir := t.TempDir()
	src := `package sample

import copier "github.com/Alex41/copier-gen"

type Source struct {
	Name string
}

type Destination struct {
	Name string
}

type Wrapper struct {
	Destination Destination
}

func Cast(src Source) error {
	wrapper := &Wrapper{}
	return copier.Copy(&wrapper.Destination, src, copier.Option{})
}
`
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	m, err := loadModel(dir, nil)
	if err != nil {
		t.Fatalf("loadModel returned error: %v", err)
	}
	if len(m.pairs) != 1 {
		t.Fatalf("expected one discovered pair, got %d", len(m.pairs))
	}
	if m.pairs[0].dstName != "Destination" {
		t.Fatalf("expected Destination mapper, got %s", m.pairs[0].dstName)
	}
}

func TestLoadModelFailsIfAnyDestinationIsNotWritablePointer(t *testing.T) {
	dir := t.TempDir()
	src := `package sample

import copier "github.com/Alex41/copier-gen"

type Source struct {
	Name string
}

type Destination struct {
	Name string
}

func Valid(src Source) error {
	dst := &Destination{}
	return copier.Copy(dst, src)
}

func Invalid(src Source) error {
	var dst Destination
	return copier.Copy(dst, src)
}
`
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := loadModel(dir, nil)
	if err == nil {
		t.Fatal("loadModel succeeded with a non-pointer destination")
	}
	for _, want := range []string{
		"found copier calls that cannot be generated",
		"destination argument must be a writable pointer to a named struct",
		"sample.go:",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error does not contain %q: %v", want, err)
		}
	}
}

func TestRenderSupportsPointerToPointerDestination(t *testing.T) {
	dir := t.TempDir()
	src := `package sample

import copier "github.com/Alex41/copier-gen"

type Source struct {
	Name string
}

type Destination struct {
	Name string
}

func Cast(src Source) error {
	dst := &Destination{}
	return copier.Copy(&dst, src)
}
`
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	m, err := loadModel(dir, nil)
	if err != nil {
		t.Fatalf("loadModel returned error: %v", err)
	}
	out, err := render(m)
	if err != nil {
		t.Fatalf("render returned error: %v", err)
	}
	text := string(out)
	mapper := mapperName(m.pairs[0])
	for _, want := range []string{
		"func " + mapper + "(toValue **Destination, from Source, opt copier.Option) error",
		"if toValue == nil || *toValue == nil",
		"to := *toValue",
		"to.Name = from.Name",
		"to, ok := toValue.(**Destination)",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("generated output does not contain %q:\n%s", want, text)
		}
	}
}

func TestRenderKeepsPointerAndPointerToPointerDestinations(t *testing.T) {
	dir := t.TempDir()
	src := `package sample

import copier "github.com/Alex41/copier-gen"

type Source struct {
	Name string
}

type Destination struct {
	Name string
}

func CastDirect(dst *Destination, src Source) error {
	return copier.Copy(dst, src)
}

func CastIndirect(dst **Destination, src Source) error {
	return copier.Copy(dst, src)
}
`
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	m, err := loadModel(dir, nil)
	if err != nil {
		t.Fatalf("loadModel returned error: %v", err)
	}
	if len(m.pairs) != 2 {
		t.Fatalf("generated %d pairs, want 2", len(m.pairs))
	}
	directMapper := mapperName(m.pairs[0])
	indirectMapper := mapperName(m.pairs[1])
	if m.pairs[0].toIndirect {
		directMapper, indirectMapper = indirectMapper, directMapper
	}
	if directMapper == indirectMapper {
		t.Fatalf("direct and indirect destinations have the same mapper name %q", directMapper)
	}
	out, err := render(m)
	if err != nil {
		t.Fatalf("render returned error: %v", err)
	}
	text := string(out)
	for _, want := range []string{
		"func " + directMapper + "(to *Destination, from Source, opt copier.Option) error",
		"to, ok := toValue.(*Destination)",
		"func " + indirectMapper + "(toValue **Destination, from Source, opt copier.Option) error",
		"to, ok := toValue.(**Destination)",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("generated output does not contain %q:\n%s", want, text)
		}
	}
}

func TestRenderUsesSingleInitForAllMappersInFile(t *testing.T) {
	dir := t.TempDir()
	src := `package sample

import copier "github.com/Alex41/copier-gen"

type FirstSource struct {
	Name string
}

type FirstDestination struct {
	Name string
}

type SecondSource struct {
	Count int
}

type SecondDestination struct {
	Count int
}

func CopyFirst(dst *FirstDestination, src FirstSource) error {
	return copier.Copy(dst, src)
}

func CopySecond(dst *SecondDestination, src SecondSource) error {
	return copier.Copy(dst, src)
}
`
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	m, err := loadModel(dir, nil)
	if err != nil {
		t.Fatalf("loadModel returned error: %v", err)
	}
	out, err := render(m)
	if err != nil {
		t.Fatalf("render returned error: %v", err)
	}
	text := string(out)
	if count := strings.Count(text, "func init()"); count != 1 {
		t.Fatalf("generated %d init functions, want 1:\n%s", count, text)
	}
	if count := strings.Count(text, "copier.RegisterMapper("); count != 2 {
		t.Fatalf("generated %d mapper registrations, want 2:\n%s", count, text)
	}
	for _, pair := range m.pairs {
		if !strings.Contains(text, "return true, "+mapperName(pair)+"(to, from, opt)") {
			t.Fatalf("generated init does not register %s:\n%s", mapperName(pair), text)
		}
	}
}

func TestMapperNameIsStableHash(t *testing.T) {
	pair := copyPair{
		srcType: types.Typ[types.String],
		dstType: types.Typ[types.Int],
	}
	name := mapperName(pair)
	if !regexp.MustCompile(`^_copier_[0-9a-f]{16}$`).MatchString(name) {
		t.Fatalf("mapper name %q is not a private 64-bit hash name", name)
	}
	if mapperName(pair) != name {
		t.Fatal("mapper name is not stable")
	}
	pair.toIndirect = true
	if mapperName(pair) == name {
		t.Fatal("destination indirection is not included in mapper hash")
	}
}

func TestRenderPointerSourceToValueDestination(t *testing.T) {
	dir := t.TempDir()
	src := `package sample

import copier "github.com/Alex41/copier-gen"

type Source struct {
	Enabled *bool
}

type Destination struct {
	Enabled bool
}

func Cast(src Source) error {
	dst := &Destination{}
	return copier.Copy(dst, src, copier.Option{IgnoreEmpty: true})
}
`
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	m, err := loadModel(dir, nil)
	if err != nil {
		t.Fatalf("loadModel returned error: %v", err)
	}
	out, err := render(m)
	if err != nil {
		t.Fatalf("render returned error: %v", err)
	}
	text := string(out)
	for _, want := range []string{
		"if from.Enabled != nil",
		"to.Enabled = *from.Enabled",
		"to.Enabled = copiergen.Zero[bool]()",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("generated output does not contain %q:\n%s", want, text)
		}
	}
}

func TestRenderNestedPointerStructToValueStruct(t *testing.T) {
	dir := t.TempDir()
	src := `package sample

import copier "github.com/Alex41/copier-gen"

type Notifications[T bool | *bool] struct {
	Email T
	Push T
}

type Source struct {
	Notifications *Notifications[*bool]
}

type Destination struct {
	Notifications Notifications[bool]
}

func Cast(src Source) error {
	dst := &Destination{}
	return copier.Copy(dst, src, copier.Option{IgnoreEmpty: true})
}
`
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	m, err := loadModel(dir, nil)
	if err != nil {
		t.Fatalf("loadModel returned error: %v", err)
	}
	out, err := render(m)
	if err != nil {
		t.Fatalf("render returned error: %v", err)
	}
	text := string(out)
	nested := m.pairs[0].fields[0]
	helper := nestedMapperNameForField(nested)
	for _, want := range []string{
		"if from.Notifications != nil",
		"func " + helper + "(to *Notifications[bool], from *Notifications[*bool], opt copier.Option) error",
		"if from.Email != nil",
		"to.Email = *from.Email",
		helper + "(&to.Notifications, from.Notifications, opt)",
		"to.Notifications = copiergen.Zero[Notifications[bool]]()",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("generated output does not contain %q:\n%s", want, text)
		}
	}
}

func TestRenderNestedPointerStructToPointerStructWithConvertibleFields(t *testing.T) {
	dir := t.TempDir()
	src := `package sample

import copier "github.com/Alex41/copier-gen"

type Range[T comparable] struct {
	From T
	To T
}

type Source struct {
	FileLimit *Range[uint]
}

type Destination struct {
	FileLimit *Range[uint8]
}

func Cast(src Source) error {
	dst := &Destination{}
	return copier.Copy(dst, src)
}
`
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	m, err := loadModel(dir, nil)
	if err != nil {
		t.Fatalf("loadModel returned error: %v", err)
	}
	out, err := render(m)
	if err != nil {
		t.Fatalf("render returned error: %v", err)
	}
	text := string(out)
	nested := m.pairs[0].fields[0]
	helper := nestedMapperNameForField(nested)
	for _, want := range []string{
		"if from.FileLimit != nil",
		"to.FileLimit = new(Range[uint8])",
		"func " + helper + "(to *Range[uint8], from *Range[uint], opt copier.Option) error",
		"to.From = uint8(from.From)",
		"to.To = uint8(from.To)",
		helper + "(to.FileLimit, from.FileLimit, opt)",
		"to.FileLimit = copiergen.Zero[*Range[uint8]]()",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("generated output does not contain %q:\n%s", want, text)
		}
	}
}

func TestRenderReusesNestedMapperForMatchingFieldTypes(t *testing.T) {
	dir := t.TempDir()
	src := `package sample

import copier "github.com/Alex41/copier-gen"

type NestedSource struct {
	Name *string
	Count *int
}

type NestedDestination struct {
	Name string
	Count int
}

type Source struct {
	Primary *NestedSource
	Secondary *NestedSource
}

type Destination struct {
	Primary NestedDestination
	Secondary NestedDestination
}

func Cast(src Source) error {
	dst := &Destination{}
	return copier.Copy(dst, src)
}
`
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	m, err := loadModel(dir, nil)
	if err != nil {
		t.Fatalf("loadModel returned error: %v", err)
	}
	if len(m.pairs) != 1 || len(m.pairs[0].fields) != 2 {
		t.Fatalf("unexpected generated model: %+v", m.pairs)
	}
	helper := nestedMapperNameForField(m.pairs[0].fields[0])
	out, err := render(m)
	if err != nil {
		t.Fatalf("render returned error: %v", err)
	}
	text := string(out)
	if count := strings.Count(text, "func "+helper+"("); count != 1 {
		t.Fatalf("generated nested helper %d times, want 1:\n%s", count, text)
	}
	for _, want := range []string{
		helper + "(&to.Primary, from.Primary, opt)",
		helper + "(&to.Secondary, from.Secondary, opt)",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("generated output does not contain %q:\n%s", want, text)
		}
	}
	if count := strings.Count(text, "to.Name = *from.Name"); count != 1 {
		t.Fatalf("nested field body generated %d times, want 1:\n%s", count, text)
	}
}

func TestWriteGeneratedFilesDeclaresNestedMapperOncePerPackage(t *testing.T) {
	dir := t.TempDir()
	typesSource := `package sample

type NestedSource struct {
	Name *string
}

type NestedDestination struct {
	Name string
}

type FirstSource struct {
	Nested *NestedSource
}

type FirstDestination struct {
	Nested NestedDestination
}

type SecondSource struct {
	Nested *NestedSource
}

type SecondDestination struct {
	Nested NestedDestination
}
`
	firstSource := `package sample

import copier "github.com/Alex41/copier-gen"

func CopyFirst(dst *FirstDestination, src FirstSource) error {
	return copier.Copy(dst, src)
}
`
	secondSource := `package sample

import copier "github.com/Alex41/copier-gen"

func CopySecond(dst *SecondDestination, src SecondSource) error {
	return copier.Copy(dst, src)
}
`
	for name, src := range map[string]string{
		"types.go":  typesSource,
		"first.go":  firstSource,
		"second.go": secondSource,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0644); err != nil {
			t.Fatal(err)
		}
	}

	m, err := loadModel(dir, nil)
	if err != nil {
		t.Fatalf("loadModel returned error: %v", err)
	}
	if err := writeGeneratedFiles(dir, "", m); err != nil {
		t.Fatalf("writeGeneratedFiles returned error: %v", err)
	}

	helper := nestedMapperNameForField(m.pairs[0].fields[0])
	var declarations, calls int
	for _, name := range []string{"first_copier_gen.go", "second_copier_gen.go"} {
		src, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		text := string(src)
		declarations += strings.Count(text, "func "+helper+"(")
		calls += strings.Count(text, helper+"(")
	}
	if declarations != 1 {
		t.Fatalf("nested helper declared %d times across generated package, want 1", declarations)
	}
	if calls != 3 {
		t.Fatalf("nested helper referenced %d times including its declaration, want 3", calls)
	}
}

func TestRenderNestedMapperUsesCallScopedConverter(t *testing.T) {
	dir := t.TempDir()
	src := `package sample

import copier "github.com/Alex41/copier-gen"

type Raw struct {
	Value string
}

type NestedSource struct {
	Value Raw
}

type NestedDestination struct {
	Value *string
}

type Source struct {
	Nested *NestedSource
}

type Destination struct {
	Nested NestedDestination
}

func ConvertRaw(src Raw) (string, error) {
	return src.Value, nil
}

func Cast(dst *Destination, src Source) error {
	return copier.Copy(dst, src, copier.Option{
		Converters: copier.Converters{
			copier.UseConverter(ConvertRaw),
		},
	})
}
`
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	m, err := loadModel(dir, nil)
	if err != nil {
		t.Fatalf("loadModel returned error: %v", err)
	}
	out, err := render(m)
	if err != nil {
		t.Fatalf("render returned error: %v", err)
	}
	text := string(out)
	helper := nestedMapperNameForField(m.pairs[0].fields[0])
	for _, want := range []string{
		"func " + helper + "(to *NestedDestination, from *NestedSource, opt copier.Option) error",
		"converter, ok := copier.FindConverter[Raw, string](opt.Converters)",
		"converted, err := converter(from.Value)",
		"to.Value = &converted",
		helper + "(&to.Nested, from.Nested, opt)",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("generated output does not contain %q:\n%s", want, text)
		}
	}
}

func TestLoadModelFailsWhenNestedMapperConverterIsMissing(t *testing.T) {
	dir := t.TempDir()
	src := `package sample

import copier "github.com/Alex41/copier-gen"

type Raw struct {
	Value string
}

type NestedSource struct {
	Value Raw
}

type NestedDestination struct {
	Value string
}

type Source struct {
	Nested *NestedSource
}

type Destination struct {
	Nested NestedDestination
}

func Cast(dst *Destination, src Source) error {
	return copier.Copy(dst, src)
}
`
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := loadModel(dir, nil)
	if err == nil {
		t.Fatal("loadModel succeeded without required nested converter")
	}
	if !strings.Contains(err.Error(), "field Nested needs converter") {
		t.Fatalf("error does not explain missing nested converter: %v", err)
	}
}

func TestWriteGeneratedFilesUsesSourceFileNames(t *testing.T) {
	dir := t.TempDir()
	m := model{
		pkgName: "sample",
		pkgPath: "sample",
		pairs: []copyPair{
			minimalPair("ActivitySource", "ActivityDestination", "activity.go"),
			minimalPair("UserProjectSource", "UserProjectDestination", "user_project.go"),
		},
	}

	if err := writeGeneratedFiles(dir, "", m); err != nil {
		t.Fatalf("writeGeneratedFiles returned error: %v", err)
	}
	for _, file := range []string{"activity_copier_gen.go", "user_project_copier_gen.go"} {
		if _, err := os.Stat(filepath.Join(dir, file)); err != nil {
			t.Fatalf("expected generated file %s: %v", file, err)
		}
	}
}

func TestRenderUsesTypedConverterForIncompatibleField(t *testing.T) {
	dir := t.TempDir()
	src := `package sample

import copier "github.com/Alex41/copier-gen"

type Raw struct {
	Value string
}

type Formatted struct {
	Label string
}

type Source struct {
	Status Raw
}

type Destination struct {
	Status Formatted
}

func ConvertRaw(src Raw) (Formatted, error) {
	return Formatted{Label: src.Value}, nil
}

func Cast(src Source) error {
	dst := &Destination{}
	return copier.Copy(dst, src, copier.Option{
		Converters: copier.Converters{
			copier.UseConverter[Raw, Formatted](ConvertRaw),
		},
	})
}
`
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	m, err := loadModel(dir, nil)
	if err != nil {
		t.Fatalf("loadModel returned error: %v", err)
	}
	out, err := render(m)
	if err != nil {
		t.Fatalf("render returned error: %v", err)
	}
	text := string(out)
	for _, want := range []string{
		"converter, ok := copier.FindConverter[Raw, Formatted](opt.Converters)",
		"if !ok {",
		"converted, err := converter(from.Status)",
		"to.Status = converted",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("generated output does not contain %q:\n%s", want, text)
		}
	}
}

func TestRenderUsesTypedContextConverterForIncompatibleField(t *testing.T) {
	dir := t.TempDir()
	src := `package sample

import (
	"context"

	copier "github.com/Alex41/copier-gen"
)

type Raw struct {
	Value string
}

type Formatted struct {
	Label string
}

type Source struct {
	Status Raw
}

type Destination struct {
	Status Formatted
}

func ConvertRaw(ctx context.Context, src Raw) (Formatted, error) {
	return Formatted{Label: src.Value}, nil
}

func Cast(ctx context.Context, src Source) error {
	dst := &Destination{}
	return copier.Copy(dst, src, copier.Option{
		Context: ctx,
		Converters: copier.Converters{
			copier.UseConverterContext[Raw, Formatted](ConvertRaw),
		},
	})
}
`
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	m, err := loadModel(dir, nil)
	if err != nil {
		t.Fatalf("loadModel returned error: %v", err)
	}
	out, err := render(m)
	if err != nil {
		t.Fatalf("render returned error: %v", err)
	}
	text := string(out)
	for _, want := range []string{
		"converter, ok := copier.FindConverterContext[Raw, Formatted](opt.Converters)",
		"converted, err := converter(opt.Context, from.Status)",
		"to.Status = converted",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("generated output does not contain %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "FindConverter[Raw, Formatted]") ||
		strings.Contains(text, "converter(from.Status)") {
		t.Fatalf("generated output used ordinary converter path:\n%s", text)
	}
}

func TestRenderInfersContextConverterTypes(t *testing.T) {
	dir := t.TempDir()
	src := `package sample

import (
	"context"

	copier "github.com/Alex41/copier-gen"
)

type Raw struct {
	Value string
}

type Source struct {
	Status Raw
}

type Destination struct {
	Status string
}

func Cast(ctx context.Context, src Source) error {
	dst := &Destination{}
	return copier.Copy(dst, src, copier.Option{
		Context: ctx,
		Converters: copier.Converters{
			copier.UseConverterContext(func(ctx context.Context, src Raw) (string, error) {
				return src.Value, nil
			}),
		},
	})
}
`
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	m, err := loadModel(dir, nil)
	if err != nil {
		t.Fatalf("loadModel returned error: %v", err)
	}
	out, err := render(m)
	if err != nil {
		t.Fatalf("render returned error: %v", err)
	}
	text := string(out)
	for _, want := range []string{
		"converter, ok := copier.FindConverterContext[Raw, string](opt.Converters)",
		"converted, err := converter(opt.Context, from.Status)",
		"to.Status = converted",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("generated output does not contain %q:\n%s", want, text)
		}
	}
}

func TestRenderUsesValueConverterForPointerDestination(t *testing.T) {
	dir := t.TempDir()
	src := `package sample

import copier "github.com/Alex41/copier-gen"

type Raw struct {
	Value string
}

type Formatted struct {
	Label string
}

type Source struct {
	Status Raw
}

type Destination struct {
	Status *Formatted
}

func ConvertRaw(src Raw) (Formatted, error) {
	return Formatted{Label: src.Value}, nil
}

func Cast(src Source) error {
	dst := &Destination{}
	return copier.Copy(dst, src, copier.Option{
		Converters: copier.Converters{
			copier.UseConverter[Raw, Formatted](ConvertRaw),
		},
	})
}
`
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	m, err := loadModel(dir, nil)
	if err != nil {
		t.Fatalf("loadModel returned error: %v", err)
	}
	out, err := render(m)
	if err != nil {
		t.Fatalf("render returned error: %v", err)
	}
	text := string(out)
	for _, want := range []string{
		"converter, ok := copier.FindConverter[Raw, Formatted](opt.Converters)",
		"converted, err := converter(from.Status)",
		"to.Status = &converted",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("generated output does not contain %q:\n%s", want, text)
		}
	}
}

func TestRenderPrefersExactConverterOverDirectAssignment(t *testing.T) {
	dir := t.TempDir()
	src := `package sample

import copier "github.com/Alex41/copier-gen"

type Source struct {
	Title *string
}

type Destination struct {
	Title *string
}

func Cast(src Source) error {
	dst := &Destination{}
	return copier.Copy(dst, src, copier.Option{
		Converters: copier.Converters{
			copier.UseConverter(func(src *string) (*string, error) {
				if src == nil || *src == "" {
					return nil, nil
				}
				return src, nil
			}),
		},
	})
}
`
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	m, err := loadModel(dir, nil)
	if err != nil {
		t.Fatalf("loadModel returned error: %v", err)
	}
	out, err := render(m)
	if err != nil {
		t.Fatalf("render returned error: %v", err)
	}
	text := string(out)
	for _, want := range []string{
		"converter, ok := copier.FindConverter[*string, *string](opt.Converters)",
		"converted, err := converter(from.Title)",
		"to.Title = converted",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("generated output does not contain %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "to.Title = from.Title") {
		t.Fatalf("generated output used direct assignment instead of converter:\n%s", text)
	}
}

func TestRenderDeepCopyPointerField(t *testing.T) {
	dir := t.TempDir()
	src := `package sample

import copier "github.com/Alex41/copier-gen"

type Source struct {
	Title *string
}

type Destination struct {
	Title *string
}

func Cast(src Source) error {
	dst := &Destination{}
	return copier.Copy(dst, src, copier.Option{DeepCopy: true})
}
`
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	m, err := loadModel(dir, nil)
	if err != nil {
		t.Fatalf("loadModel returned error: %v", err)
	}
	out, err := render(m)
	if err != nil {
		t.Fatalf("render returned error: %v", err)
	}
	text := string(out)
	for _, want := range []string{
		"if from.Title != nil",
		"copied := *from.Title",
		"to.Title = &copied",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("generated output does not contain %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "to.Title = from.Title") {
		t.Fatalf("generated output used shallow pointer assignment:\n%s", text)
	}
}

func TestRenderDeepCopyTimePointerField(t *testing.T) {
	dir := t.TempDir()
	src := `package sample

import (
	"time"

	copier "github.com/Alex41/copier-gen"
)

type Source struct {
	Birthdate *time.Time
}

type Destination struct {
	Birthdate *time.Time
}

func Cast(src Source) error {
	dst := &Destination{}
	return copier.Copy(dst, src, copier.Option{DeepCopy: true})
}
`
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	m, err := loadModel(dir, nil)
	if err != nil {
		t.Fatalf("loadModel returned error: %v", err)
	}
	out, err := render(m)
	if err != nil {
		t.Fatalf("render returned error: %v", err)
	}
	text := string(out)
	for _, want := range []string{
		`"time"`,
		"if from.Birthdate != nil",
		"copied := *from.Birthdate",
		"to.Birthdate = &copied",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("generated output does not contain %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "to.Birthdate = from.Birthdate") {
		t.Fatalf("generated output used shallow time pointer assignment:\n%s", text)
	}
}

func TestRenderDeepCopyPointerToValueField(t *testing.T) {
	dir := t.TempDir()
	src := `package sample

import copier "github.com/Alex41/copier-gen"

type Source struct {
	Enabled *bool
}

type Destination struct {
	Enabled bool
}

func Cast(src Source) error {
	dst := &Destination{}
	return copier.Copy(dst, src, copier.Option{DeepCopy: true})
}
`
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	m, err := loadModel(dir, nil)
	if err != nil {
		t.Fatalf("loadModel returned error: %v", err)
	}
	out, err := render(m)
	if err != nil {
		t.Fatalf("render returned error: %v", err)
	}
	text := string(out)
	for _, want := range []string{
		"if from.Enabled != nil",
		"to.Enabled = *from.Enabled",
		"to.Enabled = copiergen.Zero[bool]()",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("generated output does not contain %q:\n%s", want, text)
		}
	}
}

func TestRenderCopiesFieldsFromEmbeddedSourceStruct(t *testing.T) {
	dir := t.TempDir()
	src := `package sample

import copier "github.com/Alex41/copier-gen"

type EditCore struct {
	Title *string
	Skip *string ` + "`copier:\"-\"`" + `
}

type Source struct {
	EditCore ` + "`json:\",inline\"`" + `
	Enabled *bool
}

type Destination struct {
	Title string
	Skip string
	Enabled bool
}

func Cast(src Source) error {
	dst := &Destination{}
	return copier.Copy(dst, src, copier.Option{
		IgnoreEmpty: true,
		DeepCopy: true,
	})
}
`
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	m, err := loadModel(dir, nil)
	if err != nil {
		t.Fatalf("loadModel returned error: %v", err)
	}
	out, err := render(m)
	if err != nil {
		t.Fatalf("render returned error: %v", err)
	}
	text := string(out)
	for _, want := range []string{
		"if from.EditCore.Title != nil",
		"to.Title = *from.EditCore.Title",
		"if from.Enabled != nil",
		"to.Enabled = *from.Enabled",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("generated output does not contain %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "from.EditCore.Skip") || strings.Contains(text, "to.Skip") {
		t.Fatalf("generated output copied ignored embedded field:\n%s", text)
	}
}

func TestRenderDeepCopySliceField(t *testing.T) {
	dir := t.TempDir()
	src := `package sample

import copier "github.com/Alex41/copier-gen"

type Source struct {
	Counts []uint
}

type Destination struct {
	Counts []uint8
}

func Cast(src Source) error {
	dst := &Destination{}
	return copier.Copy(dst, src, copier.Option{DeepCopy: true})
}
`
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	m, err := loadModel(dir, nil)
	if err != nil {
		t.Fatalf("loadModel returned error: %v", err)
	}
	out, err := render(m)
	if err != nil {
		t.Fatalf("render returned error: %v", err)
	}
	text := string(out)
	for _, want := range []string{
		"copied := make([]uint8, 0, len(from.Counts))",
		"for _, item := range from.Counts",
		"copied = append(copied, uint8(item))",
		"to.Counts = copied",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("generated output does not contain %q:\n%s", want, text)
		}
	}
}

func TestRenderDeepCopyNestedPointerSliceField(t *testing.T) {
	dir := t.TempDir()
	src := `package sample

import copier "github.com/Alex41/copier-gen"

type Raw struct {
	Value string
}

type ChildSource struct {
	Image Raw
}

type ChildDestination struct {
	Image *string
}

type Source struct {
	Children []*ChildSource
}

type Destination struct {
	Children []ChildDestination
}

func ConvertRaw(src Raw) (string, error) {
	return src.Value, nil
}

func Cast(src Source) error {
	dst := &Destination{}
	return copier.Copy(dst, src, copier.Option{
		DeepCopy: true,
		Converters: copier.Converters{
			copier.UseConverter(ConvertRaw),
		},
	})
}
`
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	m, err := loadModel(dir, nil)
	if err != nil {
		t.Fatalf("loadModel returned error: %v", err)
	}
	out, err := render(m)
	if err != nil {
		t.Fatalf("render returned error: %v", err)
	}
	text := string(out)
	helper := nestedMapperNameForField(m.pairs[0].fields[0])
	for _, want := range []string{
		"copied := make([]ChildDestination, 0, len(from.Children))",
		"for _, item := range from.Children",
		"var convertedItem ChildDestination",
		"if err := " + helper + "(&convertedItem, item, opt); err != nil {",
		"copied = append(copied, convertedItem)",
		"to.Children = copied",
		"converter, ok := copier.FindConverter[Raw, string](opt.Converters)",
		"to.Image = &converted",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("generated output does not contain %q:\n%s", want, text)
		}
	}
}

func TestLoadModelFailsWhenDeepCopyCannotBeGenerated(t *testing.T) {
	dir := t.TempDir()
	src := `package sample

import copier "github.com/Alex41/copier-gen"

type Source struct {
	Values map[string]string
}

type Destination struct {
	Values map[string]string
}

func Cast(src Source) error {
	dst := &Destination{}
	return copier.Copy(dst, src, copier.Option{DeepCopy: true})
}
`
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := loadModel(dir, nil)
	if err == nil {
		t.Fatal("loadModel succeeded for unsupported deep copy")
	}
	if !strings.Contains(err.Error(), "cannot generate deep copy mapper") {
		t.Fatalf("error does not explain unsupported deep copy: %v", err)
	}
}

func TestRenderUsesInferredElementConverterForSliceField(t *testing.T) {
	dir := t.TempDir()
	src := `package sample

import copier "github.com/Alex41/copier-gen"

type Raw struct {
	Value string
}

type Formatted struct {
	Label string
}

type Source struct {
	Points []*Raw
}

type Destination struct {
	Points []Formatted
}

func Cast(src Source) error {
	dst := &Destination{}
	return copier.Copy(dst, src, copier.Option{
		Converters: copier.Converters{
			copier.UseConverter(func(s *Raw) (Formatted, error) {
				return Formatted{Label: s.Value}, nil
			}),
		},
	})
}
`
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	m, err := loadModel(dir, nil)
	if err != nil {
		t.Fatalf("loadModel returned error: %v", err)
	}
	out, err := render(m)
	if err != nil {
		t.Fatalf("render returned error: %v", err)
	}
	text := string(out)
	for _, want := range []string{
		"converter, ok := copier.FindConverter[*Raw, Formatted](opt.Converters)",
		"converted = make([]Formatted, 0, len(from.Points))",
		"for _, item := range from.Points",
		"convertedItem, err := converter(item)",
		"to.Points = converted",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("generated output does not contain %q:\n%s", want, text)
		}
	}
}

func TestRenderUsesPointerElementConverterFallbacksForSliceField(t *testing.T) {
	tests := []struct {
		name      string
		converter string
		want      []string
	}{
		{
			name: "exact converter wins",
			converter: `copier.UseConverter(func(s string) (Formatted, error) {
				return Formatted{Label: s}, nil
			}),
			copier.UseConverter(func(s *string) (Formatted, error) {
				return Formatted{Label: *s}, nil
			}),`,
			want: []string{
				"converter, ok := copier.FindConverter[string, Formatted](opt.Converters)",
				"convertedItem, err := converter(item)",
				"converted = append(converted, convertedItem)",
			},
		},
		{
			name: "pointer source",
			converter: `copier.UseConverter(func(s *string) (Formatted, error) {
				return Formatted{Label: *s}, nil
			}),`,
			want: []string{
				"converter, ok := copier.FindConverter[*string, Formatted](opt.Converters)",
				"convertedItem, err := converter(&item)",
				"converted = append(converted, convertedItem)",
			},
		},
		{
			name: "pointer source and destination",
			converter: `copier.UseConverter(func(s *string) (*Formatted, error) {
				return &Formatted{Label: *s}, nil
			}),`,
			want: []string{
				"converter, ok := copier.FindConverter[*string, *Formatted](opt.Converters)",
				"convertedItem, err := converter(&item)",
				"if convertedItem == nil {",
				"converted = append(converted, copiergen.Zero[Formatted]())",
				"converted = append(converted, *convertedItem)",
			},
		},
		{
			name: "pointer destination",
			converter: `copier.UseConverter(func(s string) (*Formatted, error) {
				return &Formatted{Label: s}, nil
			}),`,
			want: []string{
				"converter, ok := copier.FindConverter[string, *Formatted](opt.Converters)",
				"convertedItem, err := converter(item)",
				"if convertedItem == nil {",
				"converted = append(converted, copiergen.Zero[Formatted]())",
				"converted = append(converted, *convertedItem)",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			src := strings.ReplaceAll(`package sample

import copier "github.com/Alex41/copier-gen"

type Formatted struct {
	Label string
}

type Source struct {
	Points []string
}

type Destination struct {
	Points []Formatted
}

func Cast(src Source) error {
	dst := &Destination{}
	return copier.Copy(dst, src, copier.Option{
		Converters: copier.Converters{
			__CONVERTER__
		},
	})
}
`, "__CONVERTER__", tt.converter)
			if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(src), 0644); err != nil {
				t.Fatal(err)
			}

			m, err := loadModel(dir, nil)
			if err != nil {
				t.Fatalf("loadModel returned error: %v", err)
			}
			out, err := render(m)
			if err != nil {
				t.Fatalf("render returned error: %v", err)
			}
			text := string(out)
			for _, want := range tt.want {
				if !strings.Contains(text, want) {
					t.Fatalf("generated output does not contain %q:\n%s", want, text)
				}
			}
		})
	}
}

func TestRenderUsesPointerElementConverterInsideNestedSlice(t *testing.T) {
	dir := t.TempDir()
	src := `package sample

import copier "github.com/Alex41/copier-gen"

type FileInfo struct {
	Path string
}

type Question struct {
	Files []string
}

type Survey struct {
	Questions []*Question
}

type QuestionResponse struct {
	Files []FileInfo
}

type SurveyResponse struct {
	Questions []QuestionResponse
}

func Cast(src *Survey) error {
	dst := &SurveyResponse{}
	return copier.Copy(dst, src, copier.Option{
		DeepCopy: true,
		Converters: copier.Converters{
			copier.UseConverter(func(s *string) (FileInfo, error) {
				return FileInfo{Path: *s}, nil
			}),
		},
	})
}
`
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	m, err := loadModel(dir, nil)
	if err != nil {
		t.Fatalf("loadModel returned error: %v", err)
	}
	out, err := render(m)
	if err != nil {
		t.Fatalf("render returned error: %v", err)
	}
	text := string(out)
	for _, want := range []string{
		"converter, ok := copier.FindConverter[*string, FileInfo](opt.Converters)",
		"for _, item := range from.Questions",
		"for _, item := range from.Files",
		"convertedItem, err := converter(&item)",
		"converted = append(converted, convertedItem)",
		"to.Files = converted",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("generated output does not contain %q:\n%s", want, text)
		}
	}
}

func TestLoadModelFailsWhenRequiredConverterIsMissing(t *testing.T) {
	dir := t.TempDir()
	src := `package sample

import copier "github.com/Alex41/copier-gen"

type Raw struct {
	Value string
}

type Formatted struct {
	Label string
}

type Source struct {
	Status Raw
}

type Destination struct {
	Status Formatted
}

func Cast(src Source) error {
	dst := &Destination{}
	return copier.Copy(dst, src, copier.Option{})
}
`
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := loadModel(dir, nil)
	if err == nil {
		t.Fatal("loadModel succeeded without required converter")
	}
	if !strings.Contains(err.Error(), "needs converter") {
		t.Fatalf("error does not explain missing converter: %v", err)
	}
}

func TestLoadModelIgnoresDashTaggedDestinationField(t *testing.T) {
	dir := t.TempDir()
	src := `package sample

import copier "github.com/Alex41/copier-gen"

type Raw struct {
	Value string
}

type Formatted struct {
	Label string
}

type Source struct {
	Ignored Raw
}

type Destination struct {
	Ignored Formatted ` + "`copier:\"-\"`" + `
}

func Cast(src Source) error {
	dst := &Destination{}
	return copier.Copy(dst, src, copier.Option{})
}
`
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	m, err := loadModel(dir, nil)
	if err != nil {
		t.Fatalf("loadModel returned error for ignored field: %v", err)
	}
	out, err := render(m)
	if err != nil {
		t.Fatalf("render returned error: %v", err)
	}
	if strings.Contains(string(out), "to.Ignored") {
		t.Fatalf("generated output copies ignored field:\n%s", string(out))
	}
}

func TestLoadModelIgnoresDashTaggedSourceField(t *testing.T) {
	dir := t.TempDir()
	src := `package sample

import copier "github.com/Alex41/copier-gen"

type Raw struct {
	Value string
}

type Formatted struct {
	Label string
}

type Source struct {
	Ignored Raw ` + "`copier:\"-\"`" + `
}

type Destination struct {
	Ignored Formatted
}

func Cast(src Source) error {
	dst := &Destination{}
	return copier.Copy(dst, src, copier.Option{})
}
`
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	m, err := loadModel(dir, nil)
	if err != nil {
		t.Fatalf("loadModel returned error for ignored source field: %v", err)
	}
	out, err := render(m)
	if err != nil {
		t.Fatalf("render returned error: %v", err)
	}
	if strings.Contains(string(out), "from.Ignored") || strings.Contains(string(out), "to.Ignored") {
		t.Fatalf("generated output copies ignored source field:\n%s", string(out))
	}
}

func minimalPair(srcName, dstName, sourceFile string) copyPair {
	srcStruct := types.NewStruct(nil, nil)
	dstStruct := types.NewStruct(nil, nil)
	pkg := types.NewPackage("sample", "sample")
	src := types.NewNamed(types.NewTypeName(token.NoPos, pkg, srcName, nil), srcStruct, nil)
	dst := types.NewNamed(types.NewTypeName(token.NoPos, pkg, dstName, nil), dstStruct, nil)
	return copyPair{
		srcName:    srcName,
		dstName:    dstName,
		srcType:    src,
		dstType:    dst,
		src:        srcStruct,
		dst:        dstStruct,
		sourceFile: sourceFile,
	}
}
