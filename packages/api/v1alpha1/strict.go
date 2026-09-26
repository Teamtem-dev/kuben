package v1alpha1

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
)

// required lists, per JSON object shape, the members serde refused to go
// without (Rust fields with neither #[serde(default)] nor Option), and the
// nested shapes to check below them.
type required struct {
	members []string
	objects map[string]required // member → the object it holds
	lists   map[string]required // member → the shape of each element
}

// appSpecShape is the required members of AppSpec and everything in it.
func appSpecShape() required {
	keyRef := required{members: []string{"name", "key"}}
	return required{
		members: []string{"source", "runtime"},
		objects: map[string]required{
			"source": {objects: map[string]required{"git": {members: []string{"repo"}}}},
			"runtime": {
				members: []string{"processes"},
				objects: map[string]required{"healthCheck": {members: []string{"path"}}},
			},
		},
		lists: map[string]required{
			"env": {
				members: []string{"name"},
				objects: map[string]required{"fromSecret": keyRef, "fromService": keyRef},
			},
			"domains": {members: []string{"host"}},
			"volumes": {members: []string{"name", "mountPath"}},
		},
	}
}

// DecodeAppSpec decodes an AppSpec as strictly as serde did: a required
// member that is missing or null is an error ("missing field `runtime`"),
// where encoding/json would leave its zero value. Defaults are applied as
// by UnmarshalJSON.
func DecodeAppSpec(data []byte) (AppSpec, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var generic any
	if err := d.Decode(&generic); err != nil {
		return AppSpec{}, err
	}
	if err := appSpecShape().check(generic); err != nil {
		return AppSpec{}, err
	}
	var spec AppSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return AppSpec{}, err
	}
	return spec, nil
}

// check reports the first required member v lacks. Values of another type
// than an object or a list are left to the typed decoding, which refuses
// them.
func (r required) check(v any) error {
	object, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	for _, m := range r.members {
		if value, present := object[m]; !present || value == nil {
			return fmt.Errorf("missing field `%s`", m)
		}
	}
	for _, m := range slices.Sorted(maps.Keys(r.objects)) {
		if err := r.objects[m].check(object[m]); err != nil {
			return err
		}
	}
	for _, m := range slices.Sorted(maps.Keys(r.lists)) {
		shape := r.lists[m]
		items, ok := object[m].([]any)
		if !ok {
			continue
		}
		for _, item := range items {
			if err := shape.check(item); err != nil {
				return err
			}
		}
	}
	return nil
}
