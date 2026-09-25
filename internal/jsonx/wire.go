// Package wire decodes JSON as strictly as serde did: a member the Rust type
// required must be present and not null, and absent and null are the same
// for an optional one. Contract-exact encoding (canonical JSON for content
// hashes) joins this package with the render port.
package wire

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/Teamtem-dev/kuben/internal/core/opt"
)

// Object is a JSON object whose members are decoded one by one.
type Object = map[string]json.RawMessage

// IsNull reports whether raw is the JSON literal null.
func IsNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// MissingError is a member that had to be there.
type MissingError struct{ Field string }

func (e *MissingError) Error() string { return "missing field `" + e.Field + "`" }

// Required decodes member key of o into out; it must be present and not null.
func Required[T any](o Object, key string, out *T) error {
	raw, ok := o[key]
	if !ok || IsNull(raw) {
		return &MissingError{Field: key}
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("field `%s`: %w", key, err)
	}
	return nil
}

// Optional decodes member key of o into out; absent and null are absent.
func Optional[T any](o Object, key string, out *opt.Val[T]) error {
	raw, ok := o[key]
	if !ok || IsNull(raw) {
		*out = opt.None[T]()
		return nil
	}
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return fmt.Errorf("field `%s`: %w", key, err)
	}
	*out = opt.Some(v)
	return nil
}

// Must is a struct field serde refused to miss: decoding keeps null and
// absent apart from a value, and [Must.Get] turns them into a MissingError.
type Must[T any] struct{ opt.Val[T] }

// Get is the value, or a MissingError naming field.
func (m Must[T]) Get(field string) (T, error) {
	v, ok := m.Val.Get()
	if !ok {
		return v, &MissingError{Field: field}
	}
	return v, nil
}

// Take is [Required] that also removes key from o once read, so what is
// left of o is the rest of the payload (serde's `flatten`).
func Take[T any](o Object, key string, out *T) error {
	if err := Required(o, key, out); err != nil {
		return err
	}
	delete(o, key)
	return nil
}

// TakeOptional is [Optional] that always removes key from o.
func TakeOptional[T any](o Object, key string, out *opt.Val[T]) error {
	defer delete(o, key)
	return Optional(o, key, out)
}

// DecodeAny reads JSON text into a generic value the way serde_json::Value
// holds it: numbers keep their text (json.Number), so an integer stays an
// integer and `1.0` stays a float when the value is written or hashed
// again (see Canonical). Every JSON that can reach a content hash, a stored
// jsonb or a manifest is read with it, never with json.Unmarshal into any.
func DecodeAny(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("decode JSON: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("decode JSON: trailing data")
	}
	return v, nil
}
