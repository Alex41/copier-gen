package copiergen

import (
	"github.com/Alex41/copier-gen"
)

const tagOverride uint8 = 1 << 1

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
func ShouldIgnoreEmpty(isZero bool, flags uint8, opt copier.Option) bool {
	return opt.IgnoreEmpty && flags&tagOverride == 0 && isZero
}
