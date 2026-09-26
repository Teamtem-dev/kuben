package oci

import (
	"context"
	"maps"
	"slices"

	"github.com/Teamtem-dev/kuben/internal/core/artifact"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
)

// Fixed gives fixed answers, for tests and installations without registry
// access (Rust FixedImages): an image reference → the digest it names.
// References not listed resolve only when they carry a digest. The nil
// Fixed knows no image.
type Fixed map[string]artifact.Digest

// pinned is the resolution of a reference pinned by digest (it names itself),
// and false for a tag.
func pinned(image string) (Resolved, bool, error) {
	r, err := Parse(image)
	if err != nil {
		return Resolved{}, false, err
	}
	switch ref := r.Reference.(type) {
	case Digest:
		return Resolved{Repository: r.Repository(), Digest: ref.Digest, Given: image}, true, nil
	case Tag:
		return Resolved{}, false, nil
	case nil:
		return Resolved{}, false, Invalid{Image: image}
	}
	return Resolved{}, false, Invalid{Image: image}
}

// ResolveAs answers from the map; `nginx:1.27` and
// `docker.io/library/nginx:1.27` are one image. The login is not needed.
func (f Fixed) ResolveAs(_ context.Context, image string, _ opt.Val[Login]) (Resolved, error) {
	if resolved, ok, err := pinned(image); err != nil || ok {
		return resolved, err
	}
	wanted, err := Parse(image)
	if err != nil {
		return Resolved{}, err
	}
	digest, ok := f[image]
	if !ok {
		// The keys in order, as the Rust BTreeMap was searched: the first
		// spelling of the same image wins.
		for _, key := range slices.Sorted(maps.Keys(f)) {
			r, err := Parse(key)
			if err == nil && r.Repository() == wanted.Repository() && r.Reference == wanted.Reference {
				digest, ok = f[key]
				break
			}
		}
	}
	if !ok {
		return Resolved{}, NotFound{Image: image}
	}
	return Resolved{Repository: wanted.Repository(), Digest: digest, Given: image}, nil
}

// ListTags is the tags of the entries of repository, in the order of their
// keys. The login is not needed.
func (f Fixed) ListTags(_ context.Context, repository string, _ opt.Val[Login]) ([]string, error) {
	wanted, err := Parse(repository)
	if err != nil {
		return nil, err
	}
	tags := make([]string, 0, len(f))
	for _, key := range slices.Sorted(maps.Keys(f)) {
		r, err := Parse(key)
		if err != nil || r.Repository() != wanted.Repository() {
			continue
		}
		switch ref := r.Reference.(type) {
		case Tag:
			tags = append(tags, ref.Name)
		case Digest, nil:
		}
	}
	return tags, nil
}
