package main

import (
	"go/token"
	"go/types"
	"os"
	"path/filepath"
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
	err := copier.CopyWithOption(&dst, src, copier.Option{IgnoreEmpty: true})
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
	for _, want := range []string{
		"func CopySampleUserToSampleEmployee(to *Employee, from User, opt copier.Option) error",
		"to.FullName = from.Name",
		"to.Years = from.Age",
		"if !opt.CaseSensitive",
		"to.TITLE = from.Title",
		"to.Required = from.Required",
		"copier.RegisterMapper(func(toValue interface{}, fromValue interface{}, opt copier.Option) (bool, error)",
		"func NewCopySampleUserToSampleEmployeeConverter() copier.Converter[User, Employee]",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("generated output does not contain %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "to.Secret") {
		t.Fatalf("ignored field was generated:\n%s", text)
	}
	if strings.Contains(text, "CheckMust") {
		t.Fatalf("runtime must check was generated:\n%s", text)
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
	err := copier.CopyWithOption(&dst, src, copier.Option{})
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
	err := cg.CopyWithOption(&dst, src, cg.Option{})
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
	err := copier.CopyWithOption(&dst, src, copier.Option{})
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
	return copier.CopyWithOption(&wrapper.Destination, src, copier.Option{})
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
	return copier.CopyWithOption(dst, src, copier.Option{IgnoreEmpty: true})
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
		"to.Enabled = copier.Zero[bool]()",
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
	return copier.CopyWithOption(dst, src, copier.Option{IgnoreEmpty: true})
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
		"if from.Notifications != nil",
		"if from.Notifications.Email != nil",
		"to.Notifications.Email = *from.Notifications.Email",
		"to.Notifications = copier.Zero[Notifications[bool]]()",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("generated output does not contain %q:\n%s", want, text)
		}
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
	return copier.CopyWithOption(dst, src, copier.Option{
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
		"converted, err := ConvertRaw(from.Status)",
		"to.Status = converted",
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
	return copier.CopyWithOption(dst, src, copier.Option{})
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
	return copier.CopyWithOption(dst, src, copier.Option{})
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
	return copier.CopyWithOption(dst, src, copier.Option{})
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
