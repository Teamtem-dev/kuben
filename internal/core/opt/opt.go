// Package opt has an optional value that cannot be dereferenced when absent.
//
// Domain types use [Val] where Rust used Option<T>: a pointer would make
// every reader responsible for the nil check, and one forgotten check is a
// panic in production. A Val is read through [Val.Get], which hands out the
// presence bit together with the value.
package opt

import (
	"bytes"
	"encoding/json"
)

// Val is a T that may be absent. The zero Val is absent.
type Val[T any] struct {
	v  T
	ok bool
}

// Some is a present value.
func Some[T any](v T) Val[T] { return Val[T]{v: v, ok: true} }

// None is an absent value.
func None[T any]() Val[T] { return Val[T]{} }

// FromPtr is absent for nil and a copy of *p otherwise.
func FromPtr[T any](p *T) Val[T] {
	if p == nil {
		return Val[T]{}
	}
	return Some(*p)
}

// Get returns the value and whether it is present.
func (o Val[T]) Get() (T, bool) { return o.v, o.ok }

// IsSome reports whether the value is present.
func (o Val[T]) IsSome() bool { return o.ok }

// IsNone reports whether the value is absent.
func (o Val[T]) IsNone() bool { return !o.ok }

// Or returns the value, or fallback when it is absent.
func (o Val[T]) Or(fallback T) T {
	if o.ok {
		return o.v
	}
	return fallback
}

// Ptr is nil when absent and a pointer to a copy otherwise, for APIs that
// want one (database drivers, generated DTOs).
func (o Val[T]) Ptr() *T {
	if !o.ok {
		return nil
	}
	v := o.v
	return &v
}

// IsZero lets `json:",omitzero"` leave an absent value out.
func (o Val[T]) IsZero() bool { return !o.ok }

// MarshalJSON writes the value, or null when absent.
func (o Val[T]) MarshalJSON() ([]byte, error) {
	if !o.ok {
		return []byte("null"), nil
	}
	return json.Marshal(o.v)
}

// UnmarshalJSON reads null as absent and anything else as the value.
func (o *Val[T]) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		*o = Val[T]{}
		return nil
	}
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*o = Some(v)
	return nil
}
