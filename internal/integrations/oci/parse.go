package oci

import (
	"strings"

	"github.com/Teamtem-dev/kuben/internal/core/artifact"
)

// maxTagLen is the longest tag the OCI distribution spec allows.
const maxTagLen = 128

// Parse reads `[registry/]path[:tag][@digest]`. Without a registry the image
// is on Docker Hub, and a one-part path there is under `library/`. Without a
// tag or digest the tag is `latest`. The grammar is Kuben's contract (the
// Rust `parse`), not the one of any registry library: a path part is
// lowercase ASCII letters, digits, `.`, `_` and `-`; a tag is 1–128 ASCII
// letters, digits, `.`, `_` and `-`. It fails with an [Invalid].
func Parse(image string) (ImageRef, error) {
	invalid := Invalid{Image: image}
	name, digestText, pinned := strings.Cut(image, "@")
	var digest artifact.Digest
	if pinned {
		d, err := artifact.ParseDigest(digestText)
		if err != nil {
			return ImageRef{}, invalid
		}
		digest = d
	}
	var tag string
	tagged := false
	if i := strings.LastIndexByte(name, ':'); i >= 0 && !strings.Contains(name[i+1:], "/") {
		name, tag, tagged = name[:i], name[i+1:], true
	}
	registry, path := DockerHub, name
	if first, rest, ok := strings.Cut(name, "/"); ok &&
		(strings.ContainsAny(first, ".:") || first == "localhost") {
		registry, path = first, rest
	}
	if registry == DockerHub && !strings.Contains(path, "/") {
		path = "library/" + path
	}
	if !pathOK(path) || (tagged && !tagOK(tag)) || registry == "" {
		return ImageRef{}, invalid
	}
	var reference Reference = Tag{Name: "latest"}
	switch {
	case pinned:
		reference = Digest{Digest: digest}
	case tagged:
		reference = Tag{Name: tag}
	}
	return ImageRef{Registry: registry, Path: path, Reference: reference}, nil
}

// pathOK: non-empty parts of lowercase ASCII letters, digits, `.`, `_`, `-`.
func pathOK(path string) bool {
	if path == "" {
		return false
	}
	for part := range strings.SplitSeq(path, "/") {
		if part == "" {
			return false
		}
		for i := range len(part) {
			b := part[i]
			if (b < 'a' || b > 'z') && (b < '0' || b > '9') && b != '.' && b != '_' && b != '-' {
				return false
			}
		}
	}
	return true
}

// tagOK: 1–128 bytes of ASCII letters, digits, `.`, `_`, `-`.
func tagOK(tag string) bool {
	if tag == "" || len(tag) > maxTagLen {
		return false
	}
	for i := range len(tag) {
		b := tag[i]
		if (b < 'a' || b > 'z') && (b < 'A' || b > 'Z') && (b < '0' || b > '9') && b != '.' && b != '_' && b != '-' {
			return false
		}
	}
	return true
}
