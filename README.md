# copier-gen

`copier-gen` generates typed copy functions before build/commit time. Runtime
reflection copying is disabled; call generated functions instead.

## Usage

Add a generator directive to a package that currently calls `copier.Copy`:

```go
//go:generate go run github.com/Alex41/copier-gen/cmd/copier-gen
```

Run:

```sh
go generate ./...
```

The generator scans call sites, infers the concrete source and destination
types, writes generated files next to their source files, and creates:

```text
activity.go -> activity_copier_gen.go
```

```go
func _copierUserToEmployee(to *Employee, from User, opt copier.Option) error
```

Generated mapper functions are private and register themselves in `init()`.
Existing calls such as
`copier.Copy(&dst, src, opt)` dispatch to that generated mapper using
ordinary Go type assertions. No reflection is used.
Generated files may also import `github.com/Alex41/copier-gen/runtime` for
small helper functions used only by generated code.

`Copy` accepts an optional third argument:

```go
err := copier.Copy(&dst, src)
err := copier.Copy(&dst, src, copier.Option{IgnoreEmpty: true})
```

There is no `CopyWithOption`.

## Required Call-Site Rules

Every discovered `copier.Copy` call must be statically resolvable. If one call
cannot be generated, the entire generator run fails. Successfully resolved
calls are not written as partial output.

Destination requirements:

- the destination must be a writable pointer to a named struct
- `*Struct` is supported
- `**Struct` is supported when the caller already has `value := &Struct{}`
- a struct value, interface, map, slice, scalar, or unsupported pointer chain
  is a generation-time error
- nil destination validation remains in generated code because nil is a runtime
  value, but invalid destination types are always rejected during generation

Valid examples:

```go
var dst Employee
err := copier.Copy(&dst, src)

dst := &Employee{}
err := copier.Copy(dst, src)

dst := &Employee{}
err := copier.Copy(&dst, src) // **Employee is supported
```

Invalid example:

```go
var dst Employee
err := copier.Copy(dst, src) // generation-time error: dst is not writable
```

Source requirements:

- the source must be a named struct or pointer to a named struct
- source and destination concrete types must be inferable from the call site
- aliases for the `copier-gen` import are supported

Explicit pairs are still available for tests or manual generation:

```sh
go run github.com/Alex41/copier-gen/cmd/copier-gen -pair User:Employee
```

Use `-out some_file.go` only when you intentionally want one combined generated
file.

## Generic Converters

Converters are typed by generics and declared at generation time. There are no
`SrcType` or `DstType` marker fields.

```go
func FormatStatus(src RawStatus) (FormattedStatus, error) {
	return FormattedStatus{Value: src.Value}, nil
}

err := copier.Copy(&dst, src, copier.Option{
	Context: ctx,
	Converters: copier.Converters{
		copier.UseConverter[RawStatus, FormattedStatus](FormatStatus),
		copier.UseConverterContext(func(ctx context.Context, src RawImage) (Image, error) {
			return LoadImage(ctx, src)
		}),
	},
})
```

The generator reads `Option.Converters` from the specific `Copy` call during
`go generate`. Converters are not global runtime configuration.

Converter rules:

- an exact converter has priority over direct assignment and Go conversion
- `UseConverter[Src, Dst](Fn)` and inferred
  `UseConverter(func(Src) (Dst, error) {...})` are supported
- `UseConverterContext[Src, Dst](Fn)` and inferred
  `UseConverterContext(func(context.Context, Src) (Dst, error) {...})` are
  supported
- context-aware converters receive `Option.Context`; `Copy` uses
  `context.Background()` when no context is provided
- element converters can generate slice mappings such as `[]Src -> []Dst`
- slice element converter resolution prefers `Src -> Dst`, then falls back to
  `*Src -> Dst`, `*Src -> *Dst`, and `Src -> *Dst`
- generated code performs a typed lookup in the current call's
  `opt.Converters`; it does not use reflection
- generation prints a warning when a converter passed in a `Copy` call's
  `Option.Converters` is not used by the generated mapper
- generation prints a warning when a destination field is not written because
  no matching source field was found
- if a required converter cannot be resolved during generation, generation
  fails
- if generated code expects a call-site converter but the runtime options do
  not contain it, `ErrGeneratedConverterNotFound` is returned

## Tags And Options

Generated code supports the copier tags:

| Tag | Behavior |
| --- | --- |
| `copier:"-"` | Ignore destination field. |
| `copier:"must"` | Require the mapper to be generated for this field; generation fails if it cannot be mapped. |
| `copier:"override"` | Copy zero values even with `IgnoreEmpty`. |
| `copier:"init_slice"` | Initialize a destination slice as empty instead of nil when the source slice is nil. |
| `copier:"OtherName"` | Map fields by explicit name. |

Supported options:

| Option | Behavior |
| --- | --- |
| `IgnoreEmpty` | Skip zero source values unless the destination field has `override`. |
| `CaseSensitive` | Disable generated case-insensitive fallback matches. |
| `DeepCopy` | Generate deep copies for supported pointer/slice/nested fields; generation fails when a field would need unsupported deep copy. |
| `Converters` | Generation-time typed converter markers used to emit typed converter calls. |
| `Context` | Per-call context passed to `UseConverterContext` converters. |

## Deep Copy Rules

`DeepCopy: true` is read during generation and prevents silent shallow copies
of reference-like fields.

Supported deep-copy cases include:

- `*T -> T` by dereferencing the source
- `*T -> *T` by allocating an independent destination value
- `*time.Time -> *time.Time` with explicit value copying
- pointer-to-struct nested mapping
- recursive nested struct mapping
- slices with assignable or convertible element types
- slices using a registered element converter, including pointer/value element
  converter fallbacks
- destination slices tagged `copier:"init_slice"` are initialized as empty
  slices when the source slice is nil
- numeric conversions supported by Go, including fields inside nested generic
  structs such as `Range[uint] -> Range[uint8]`

Maps and other unsupported reference structures require an explicit converter.
If deep copying cannot be generated safely, `go generate` fails rather than
falling back to shallow assignment.

## Field Discovery

- exported fields are considered
- anonymous embedded source structs are recursively flattened for field lookup
- embedded expressions are generated explicitly, for example
  `from.EditDiscussionCore.Title`
- `copier:"-"` is respected on both source and destination fields, including
  fields inside embedded structs
- direct assignment, Go conversion, nested mapping, and converters are decided
  statically

## Generation Guarantees

- generated file names follow `<source>_copier_gen.go`
- mapper functions use deterministic private names in the form
  `_copier_<16-hex-hash>`; the hash includes source and destination pointer
  forms
- repeated nested mappings are emitted as one private typed helper per package;
  exactly one generated file owns the declaration and other generated files
  call it
- nested helper hashes include the normalized mapping plan, so mappings with
  different converter requirements do not share an incompatible helper
- nested helpers receive the current `Option` and resolve converters only from
  that `Copy` call's `Option.Converters`
- each generated file contains at most one `init()` function; it registers all
  top-level mappers emitted into that file
- generated code does not use reflection
- all output files are rendered before any file is written
- one unresolved `copier.Copy` call fails the whole run
- unsupported field mappings, missing required converters, invalid tags,
  unsupported deep copies, and non-writable destinations are generation-time
  errors

## Generator Scope

Current generator support is intentionally narrow and static:

- same-package struct-to-struct pairs via `-pair Src:Dst`
- automatic discovery of `copier.Copy` call sites
- exported fields
- direct assignment or Go type conversion
- anonymous embedded source fields
- pointer and nested struct mappings
- generated slice mappings
- registration-backed `Copy` dispatch without reflection

The extension point is the generator pipeline: new field handlers can be added
where `assignmentExpr`, tag parsing, and pair rendering decide how a field is
copied. That is where custom converters, nested structs, slices, maps, methods,
SQL Scanner/Valuer support, and field-name mapping config should be added.

Legacy inline `copier.TypeConverter{SrcType, DstType, Fn}` blocks are not kept.
Use `copier.UseConverter[Src, Dst](Fn)` inside `Option.Converters` instead.
