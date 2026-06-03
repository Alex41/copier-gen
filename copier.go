package copier

import "sync"

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

// FindConverter returns a typed converter from the current Copy options.
func FindConverter[S, D any](converters Converters) (Converter[S, D], bool) {
	for _, candidate := range converters {
		converter, ok := candidate.(Converter[S, D])
		if ok {
			return converter, true
		}
	}
	return nil, false
}

// Mapper is registered by generated files. It must return handled=false when
// the argument types do not match its generated mapper.
type Mapper func(toValue, fromValue any, opt Option) (handled bool, err error)

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

// Copy dispatches to generated typed mappers. It does not use reflection;
// generated mappers use ordinary Go type assertions.
func Copy(toValue, fromValue any, opts ...Option) error {
	opt := Option{}
	if len(opts) > 0 {
		opt = opts[0]
	}
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
