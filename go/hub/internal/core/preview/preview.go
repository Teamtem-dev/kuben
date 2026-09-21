// Package preview has preview environments (M5.1; plan §9 C14): naming,
// lifetimes and what a preview may copy from its source environment. It
// replaces the Rust module kuben-core/src/preview.rs; a preview's policy is
// policy.ForPreview.
package preview

import (
	"fmt"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/clock"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
)

const (
	// MaxTTLHours is the longest lifetime a preview may be given, in hours
	// (30 days).
	MaxTTLHours uint32 = 720

	hourMs int64 = 3_600_000
	// maxSlugLen is the longest environment name.
	maxSlugLen = 20
)

// Slug is the environment name of pull request number's preview in epoch:
// `pr<number>-<epoch>`. A reopened pull request gets a new epoch, so a new
// name and namespace. False when it would not fit an environment name.
func Slug(number, epoch uint64) (string, bool) {
	slug := fmt.Sprintf("pr%d-%d", number, epoch)
	if len(slug) > maxSlugLen {
		return "", false
	}
	return slug, true
}

// Expiry is when a preview touched at now with a lifetime of ttlHours
// expires; never earlier than current.
func Expiry(now int64, ttlHours uint32, current opt.Val[int64]) int64 {
	at := clock.SaturatingAdd(now, int64(min(max(ttlHours, 1), MaxTTLHours))*hourMs)
	if c, ok := current.Get(); ok {
		return max(c, at)
	}
	return at
}

// Extend is expiresAt pushed out by hours, but never more than [MaxTTLHours]
// beyond now.
func Extend(now, expiresAt int64, hours uint32) int64 {
	limit := clock.SaturatingAdd(now, int64(MaxTTLHours)*hourMs)
	return min(clock.SaturatingAdd(max(expiresAt, now), int64(hours)*hourMs), limit)
}

// Config is the configuration a preview's app gets from its source app:
// without secret references (a preview binds only its own environment's
// secrets, and an untrusted one none) and without custom domains (a preview
// serves its generated hostname only). What was removed is listed. The
// source is decoded JSON and is not changed.
func Config(source map[string]any) (map[string]any, []string) {
	removed := []string{}
	if source == nil {
		return nil, removed
	}
	config := cloneObject(source)
	if env, ok := config["env"].([]any); ok && env != nil {
		kept := make([]any, 0, len(env))
		for _, variable := range env {
			if secret, ok := field(variable, "fromSecret"); ok && secret != nil {
				removed = append(removed, "env "+text(variable, "name"))
				continue
			}
			kept = append(kept, variable)
		}
		config["env"] = kept
	}
	if domains, ok := config["domains"].([]any); ok && domains != nil {
		for _, domain := range domains {
			removed = append(removed, "domain "+text(domain, "host"))
		}
		config["domains"] = []any{}
	}
	if _, ok := config["imagePullSecrets"]; ok {
		delete(config, "imagePullSecrets")
		removed = append(removed, "image pull secrets")
	}
	return config, removed
}

// field is value[key] when value is an object that has the key.
func field(value any, key string) (any, bool) {
	object, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	v, ok := object[key]
	return v, ok
}

// text is the string value[key], or "?" when there is none.
func text(value any, key string) string {
	v, _ := field(value, key)
	if s, ok := v.(string); ok {
		return s
	}
	return "?"
}

func cloneObject(object map[string]any) map[string]any {
	out := make(map[string]any, len(object))
	for k, v := range object {
		out[k] = clone(v)
	}
	return out
}

// clone is a deep copy of decoded JSON; scalars are immutable and shared.
func clone(value any) any {
	switch v := value.(type) {
	case map[string]any:
		if v == nil {
			return v
		}
		return cloneObject(v)
	case []any:
		if v == nil {
			return v
		}
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = clone(item)
		}
		return out
	default:
		return value
	}
}
