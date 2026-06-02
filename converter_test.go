package copier

import "testing"

func TestGenericConverter(t *testing.T) {
	converter := Converter[int, string](func(src int) (string, error) {
		return "v" + string(rune('0'+src)), nil
	})

	got, err := Convert(7, converter)
	if err != nil {
		t.Fatalf("Convert returned error: %v", err)
	}
	if got != "v7" {
		t.Fatalf("Convert returned %q", got)
	}
}

func TestCopyWithRegisteredMapper(t *testing.T) {
	type src struct{ Name string }
	type dst struct{ Name string }

	RegisterMapper(func(toValue interface{}, fromValue interface{}, opt Option) (bool, error) {
		to, ok := toValue.(*dst)
		if !ok {
			return false, nil
		}
		from, ok := fromValue.(src)
		if !ok {
			return false, nil
		}
		to.Name = from.Name
		return true, nil
	})

	var out dst
	if err := CopyWithOption(&out, src{Name: "generated"}, Option{}); err != nil {
		t.Fatalf("CopyWithOption returned error: %v", err)
	}
	if out.Name != "generated" {
		t.Fatalf("registered mapper copied %q", out.Name)
	}
}
