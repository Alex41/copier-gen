# copier-gen

`copier-gen` generates typed copy functions before build/commit time. Runtime
reflection copying is disabled; call generated functions instead.

## Usage

Add a generator directive to a package that currently calls `copier.Copy` or
`copier.CopyWithOption`:

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
func CopyUserToEmployee(to *Employee, from User, opt copier.Option) error
func NewUserToEmployeeConverter() copier.Converter[User, Employee]
```

Generated files also register a mapper in `init()`. Existing calls such as
`copier.CopyWithOption(&dst, src, opt)` dispatch to that generated mapper using
ordinary Go type assertions. No reflection is used.

Explicit pairs are still available for tests or manual generation:

```sh
go run github.com/Alex41/copier-gen/cmd/copier-gen -pair User:Employee
```

Use `-out some_file.go` only when you intentionally want one combined generated
file.

## Generic Converters

Converters are typed by generics. There are no `SrcType` or `DstType` marker
fields.

```go
err := copier.CopyWithOption(&dst, src, copier.Option{
	Converters: []any{
		copier.Converter[RawStatus, FormattedStatus](func(src RawStatus) (FormattedStatus, error) {
			return FormatStatus(src)
		}),
	},
})
```

Generated code uses `copier.FindConverter[RawStatus, FormattedStatus](opt)` for
fields that cannot be assigned or converted directly.

## Tags And Options

Generated code supports the copier tags:

| Tag | Behavior |
| --- | --- |
| `copier:"-"` | Ignore destination field. |
| `copier:"must"` | Require the mapper to be generated for this field; generation fails if it cannot be mapped. |
| `copier:"override"` | Copy zero values even with `IgnoreEmpty`. |
| `copier:"OtherName"` | Map fields by explicit name. |

Supported options:

| Option | Behavior |
| --- | --- |
| `IgnoreEmpty` | Skip zero source values unless the destination field has `override`. |
| `CaseSensitive` | Disable generated case-insensitive fallback matches. |

`DeepCopy` is reserved in `Option`; nested copy generation is the next step.

## Generator Scope

Current generator support is intentionally narrow and static:

- same-package struct-to-struct pairs via `-pair Src:Dst`
- automatic discovery of `copier.Copy` and `copier.CopyWithOption` call sites
- exported fields
- direct assignment or Go type conversion
- registration-backed `Copy` / `CopyWithOption` dispatch without reflection
- generated typed converter factory per pair

The extension point is the generator pipeline: new field handlers can be added
where `assignmentExpr`, tag parsing, and pair rendering decide how a field is
copied. That is where custom converters, nested structs, slices, maps, methods,
SQL Scanner/Valuer support, and field-name mapping config should be added.

Legacy inline `copier.TypeConverter{SrcType, DstType, Fn}` blocks are not kept.
Use typed `copier.Converter[Src, Dst]` values in `Option.Converters`.
