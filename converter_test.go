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

func TestUseConverter(t *testing.T) {
	converter := UseConverter[int, string](func(src int) (string, error) {
		return "ok", nil
	})

	got, err := converter(1)
	if err != nil {
		t.Fatalf("converter returned error: %v", err)
	}
	if got != "ok" {
		t.Fatalf("converter returned %q", got)
	}
}

func TestCopyWithRegisteredMapper(t *testing.T) {
	type src struct{ Name string }
	type dst struct{ Name string }

	RegisterMapper(func(toValue, fromValue any, opt Option) (bool, error) {
		to, ok := toValue.(*dst)
		if !ok {
			return false, nil
		}
		from, ok := fromValue.(src)
		if !ok {
			return false, nil
		}
		if opt.IgnoreEmpty {
			return true, nil
		}
		to.Name = from.Name
		return true, nil
	})

	var out dst
	if err := Copy(&out, src{Name: "generated"}); err != nil {
		t.Fatalf("Copy returned error: %v", err)
	}
	if out.Name != "generated" {
		t.Fatalf("registered mapper copied %q", out.Name)
	}

	out = dst{}
	if err := Copy(&out, src{Name: "ignored"}, Option{IgnoreEmpty: true}); err != nil {
		t.Fatalf("Copy with option returned error: %v", err)
	}
	if out.Name != "" {
		t.Fatalf("registered mapper ignored option and copied %q", out.Name)
	}
}
