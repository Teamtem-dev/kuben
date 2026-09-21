package v1alpha1

import (
	"encoding/json"
	"fmt"
)

// parseEnum returns text as T when valid accepts it. A serde enum without
// data refuses every other string on decode, and so does this.
func parseEnum[T ~string](text, name string, valid func(T) bool) (T, error) {
	v := T(text)
	if !valid(v) {
		return "", fmt.Errorf("%s: unknown variant %q", name, text)
	}
	return v, nil
}

// unmarshalEnum decodes a JSON string into *dst through parseEnum.
func unmarshalEnum[T ~string](data []byte, dst *T, name string, valid func(T) bool) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	v, err := parseEnum(text, name, valid)
	if err != nil {
		return err
	}
	*dst = v
	return nil
}

// marshalEnum encodes v, or def when v is empty: the zero value of an enum
// whose Rust type has a #[default] variant stands for that variant. Any
// other string is refused, since the Rust type cannot hold it.
func marshalEnum[T ~string](v, def T, name string, valid func(T) bool) ([]byte, error) {
	if v == "" {
		v = def
	}
	if !valid(v) {
		return nil, fmt.Errorf("%s: unknown variant %q", name, string(v))
	}
	return json.Marshal(string(v))
}
