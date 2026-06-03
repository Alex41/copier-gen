package copier

import "sync"

// These flags are used by generated files to preserve copier tag semantics.
const (
	tagIgnore uint8 = 1 << iota
	tagOverride

	// Default values kept for source compatibility with old examples.
	String  string  = ""
	Bool    bool    = false
	Int     int     = 0
	Float32 float32 = 0
	Float64 float64 = 0
)

// Option sets copy options used by generated copy functions.
type Option struct {
	IgnoreEmpty   bool
	CaseSensitive bool
	DeepCopy      bool
	Converters    Converters
}

// Converter is a typed conversion hook. It intentionally has no SrcType/DstType
// fields: the Go type parameters are the contract.
type Converter[S, D any] func(src S) (dst D, err error)

// Converters is a generation-time list of typed converter markers.
type Converters []any

// UseConverter marks a typed converter for copier-gen. Put calls into
// Option.Converters so the generator can emit direct converter calls.
func UseConverter[S, D any](converter Converter[S, D]) Converter[S, D] {
	return converter
}

// Mapper is registered by generated files. It must return handled=false when
// the argument types do not match its generated mapper.
type Mapper func(toValue interface{}, fromValue interface{}, opt Option) (handled bool, err error)

var (
	mappersMu sync.RWMutex
	mappers   []Mapper
)

// RegisterMapper is called from generated init functions.
func RegisterMapper(mapper Mapper) {
	mappersMu.Lock()
	mappers = append(mappers, mapper)
	mappersMu.Unlock()
}

// Convert applies a typed converter.
func Convert[S, D any](src S, converter Converter[S, D]) (D, error) {
	return converter(src)
}

// IsZero is used by generated files.
func IsZero[T comparable](v T) bool {
	var zero T
	return v == zero
}

// Zero is used by generated files.
func Zero[T any]() T {
	var zero T
	return zero
}

// ShouldIgnoreEmpty mirrors copier:"override" behavior for generated code.
func ShouldIgnoreEmpty(isZero bool, flags uint8, opt Option) bool {
	return opt.IgnoreEmpty && flags&tagOverride == 0 && isZero
}

// Copy dispatches to generated typed mappers. It does not use reflection.
func Copy(toValue interface{}, fromValue interface{}) error {
	return CopyWithOption(toValue, fromValue, Option{})
}

// CopyWithOption dispatches to generated typed mappers. It does not use
// reflection; generated mappers use ordinary Go type assertions.
func CopyWithOption(toValue interface{}, fromValue interface{}, opt Option) error {
	mappersMu.RLock()
	defer mappersMu.RUnlock()
	for _, mapper := range mappers {
		handled, err := mapper(toValue, fromValue, opt)
		if handled {
			return err
		}
	}
	return ErrGeneratedMapperNotFound
}
