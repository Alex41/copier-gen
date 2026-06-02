package main

import (
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
	Required string ` + "`copier:\"must,nopanic\"`" + `
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
