package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Teamtem-dev/kuben/internal/core/ascii"

	"github.com/knadh/koanf/parsers/toml/v2"
	"github.com/knadh/koanf/providers/env/v2"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/structs"
	"github.com/knadh/koanf/v2"
)

const (
	// SystemFile is the installation-wide configuration file.
	SystemFile = "/etc/kuben/config.toml"
	// LocalFile is the configuration file of the working directory.
	LocalFile = "kuben.toml"
	// EnvPrefix starts every environment variable Kuben reads as
	// configuration; the prefix matches in any case.
	EnvPrefix = "KUBEN_"
	// EnvSeparator separates nested keys in an environment variable name.
	EnvSeparator = "__"

	// delim separates nested keys inside koanf.
	delim = "."
)

// Source is where configuration comes from. The zero Source has only the
// built-in defaults and the process environment.
type Source struct {
	// Files are TOML files, lowest precedence first. A file that does not
	// exist is skipped; one that cannot be read or parsed is an error. A
	// relative path is looked for in Dir and then in every parent of it, and
	// the nearest one is used.
	Files []string
	// Dir is where relative Files are looked for; empty for the working
	// directory.
	Dir string
	// Environ lists the environment as `NAME=value`, like [os.Environ],
	// which a nil Environ stands for. Tests pass their own.
	Environ func() []string
}

// DefaultSource is the documented precedence: defaults →
// `/etc/kuben/config.toml` → `./kuben.toml` → `KUBEN_*` environment
// variables. The CLI appends the file of `--config` to Files, which puts it
// above both files and below the environment.
func DefaultSource() Source {
	return Source{Files: []string{SystemFile, LocalFile}}
}

// Load loads the configuration using the documented precedence.
func Load() (Config, error) { return DefaultSource().Load() }

// Load loads the configuration from this source.
func (s Source) Load() (Config, error) {
	k, err := s.Koanf()
	if err != nil {
		return Config{}, err
	}
	return Extract(k)
}

// Koanf is the layered, still untyped configuration, exposed so tests and
// the CLI can layer overrides (k.Load, k.Set) before [Extract].
func (s Source) Koanf() (*koanf.Koanf, error) {
	k := koanf.New(delim)
	if err := k.Load(structs.Provider(Default(), "koanf"), nil); err != nil {
		return nil, fmt.Errorf("loading the built-in defaults: %w", err)
	}
	for _, name := range s.Files {
		path, found, err := s.find(name)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		if err := k.Load(file.Provider(path), toml.Parser()); err != nil {
			return nil, fmt.Errorf("loading %s: %w", path, err)
		}
	}
	if err := k.Load(envProvider{environ: s.Environ}, nil); err != nil {
		return nil, fmt.Errorf("loading %s* environment variables: %w", EnvPrefix, err)
	}
	return k, nil
}

// find is the file name stands for: itself when absolute, else the nearest
// one in Dir and its parents. Only regular files count.
func (s Source) find(name string) (string, bool, error) {
	if filepath.IsAbs(name) {
		return name, isFile(name), nil
	}
	dir := s.Dir
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", false, nil //nolint:nilerr // like a missing file: there is nowhere to look
		}
		dir = wd
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", false, fmt.Errorf("resolving %s: %w", s.Dir, err)
	}
	for {
		if path := filepath.Join(dir, name); isFile(path) {
			return path, true, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false, nil
		}
		dir = parent
	}
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Mode().IsRegular()
}

// envProvider is the `KUBEN_*` layer: koanf's env provider reads and
// renames the variables, and the values are parsed and nested here, because
// a variable may hold a table (`KUBEN_SSO__GROUPS={admins=admin}`) next to
// variables naming its members (`KUBEN_SSO__GROUPS__DEVS=developer`).
type envProvider struct {
	environ func() []string
}

// ReadBytes is not supported: the environment is not a document.
func (envProvider) ReadBytes() ([]byte, error) {
	return nil, errors.New("the environment provider does not support ReadBytes")
}

// Read is the nested configuration the environment holds.
func (p envProvider) Read() (map[string]any, error) {
	flat, err := env.Provider("", env.Opt{TransformFunc: envEntry, EnvironFunc: p.environ}).Read()
	if err != nil {
		return nil, err
	}
	// Shorter keys first, so that a table and its members merge the same
	// way whatever order the environment lists them in.
	keys := make([]string, 0, len(flat))
	for key := range flat {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	out := map[string]any{}
	for _, key := range keys {
		nest(out, strings.Split(key, delim), flat[key])
	}
	return out, nil
}

// envEntry turns one environment variable into a configuration key and
// value; an empty key drops it. `KUBEN_SERVER__BIND` is `server.bind`: the
// prefix goes (in any case), `__` nests, and the key is lowercased.
func envEntry(name, value string) (string, any) {
	name = strings.TrimSpace(name)
	if len(name) < len(EnvPrefix) || ascii.Lower(name[:len(EnvPrefix)]) != ascii.Lower(EnvPrefix) {
		return "", nil
	}
	key := strings.TrimSpace(strings.ReplaceAll(name[len(EnvPrefix):], EnvSeparator, delim))
	if slices.Contains(strings.Split(key, delim), "") {
		return "", nil
	}
	return ascii.Lower(key), parseEnvValue(value)
}

// nest sets path in m to v; tables merge, anything else is replaced.
func nest(m map[string]any, path []string, v any) {
	for _, part := range path[:len(path)-1] {
		next, ok := m[part].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[part] = next
		}
		m = next
	}
	last := path[len(path)-1]
	table, isTable := v.(map[string]any)
	existing, wasTable := m[last].(map[string]any)
	if !isTable || !wasTable {
		m[last] = v
		return
	}
	for key, member := range table {
		nest(existing, []string{key}, member)
	}
}
