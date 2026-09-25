package cli

// The contexts file of the client commands (cli/client.rs Context,
// Contexts, contexts_file, Session): the servers `kuben login` signed in
// to, owner-only because it holds tokens, or KUBEN_URL and KUBEN_TOKEN
// (CI).

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api/client"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/host"
)

// envLookup reads an environment variable; os.LookupEnv outside tests.
type envLookup func(string) (string, bool)

// clientTarget is where the server and project are chosen, on every client
// command: `--context`, `--project`, `--environment` (or KUBEN_CONTEXT,
// KUBEN_PROJECT, KUBEN_ENVIRONMENT).
type clientTarget struct {
	// context is a context written by `kuben login` (default: the current
	// one).
	context opt.Val[string]
	// project of apps named without one.
	project opt.Val[string]
	// environment of apps named without one.
	environment opt.Val[string]
}

// serverContext is one server signed in to. Its JSON is the file's format.
type serverContext struct {
	URL         string          `json:"url"`
	Token       string          `json:"token"`
	Project     opt.Val[string] `json:"project,omitzero"`
	Environment opt.Val[string] `json:"environment,omitzero"`
}

// contextsDoc is the contexts file.
type contextsDoc struct {
	Current  opt.Val[string]          `json:"current,omitzero"`
	Contexts map[string]serverContext `json:"contexts"`
}

// contextsFile is KUBEN_CONTEXT_FILE, else
// $XDG_CONFIG_HOME/kuben/contexts.json, else ~/.config/kuben/contexts.json.
func contextsFile(env envLookup) (string, error) {
	if file, ok := env("KUBEN_CONTEXT_FILE"); ok {
		return file, nil
	}
	if config, ok := env("XDG_CONFIG_HOME"); ok {
		return filepath.Join(config, "kuben", "contexts.json"), nil
	}
	if home, ok := env("HOME"); ok {
		return filepath.Join(home, ".config", "kuben", "contexts.json"), nil
	}
	return "", errors.New("neither XDG_CONFIG_HOME nor HOME is set; set KUBEN_CONTEXT_FILE")
}

// loadContexts reads file; a missing file is no contexts.
func loadContexts(file string) (contextsDoc, error) {
	text, err := os.ReadFile(file) //nolint:gosec // the user's own contexts file
	if errors.Is(err, fs.ErrNotExist) {
		return contextsDoc{Contexts: map[string]serverContext{}}, nil
	}
	if err != nil {
		return contextsDoc{}, fmt.Errorf("reading %s: %w", file, err)
	}
	var doc contextsDoc
	if err := json.Unmarshal(text, &doc); err != nil {
		return contextsDoc{}, fmt.Errorf("reading %s: %w", file, err)
	}
	if doc.Contexts == nil {
		doc.Contexts = map[string]serverContext{}
	}
	return doc, nil
}

// save writes the whole file, readable by its owner only: it holds tokens.
// It is written next to its place first and renamed, so a reader never
// sees half of it.
func (c contextsDoc) save(file string) error {
	if dir := filepath.Dir(file); dir != "." {
		if err := os.MkdirAll(dir, 0o777); err != nil { //nolint:gosec // narrowed to 0700 below, as Rust did
			return fmt.Errorf("creating %s: %w", dir, err)
		}
		_ = os.Chmod(dir, 0o700) //nolint:errcheck,gosec // best effort, as in Rust: a shared parent may not be ours
	}
	if c.Contexts == nil {
		c.Contexts = map[string]serverContext{}
	}
	var text bytes.Buffer
	enc := json.NewEncoder(&text)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(c); err != nil {
		return fmt.Errorf("writing %s: %w", file, err)
	}
	partial := pathWithExtension(file, "json.partial")
	// serde_json::to_string_pretty writes no final newline.
	if err := host.WriteOwnerOnly(partial, strings.TrimSuffix(text.String(), "\n")); err != nil {
		return err //nolint:wrapcheck // names the file
	}
	if err := os.Rename(partial, file); err != nil {
		return fmt.Errorf("writing %s: %w", file, err)
	}
	return nil
}

// pathWithExtension is Rust's Path::with_extension: file with its extension
// (if any) replaced by ext.
func pathWithExtension(file, ext string) string {
	base := filepath.Base(file)
	old := filepath.Ext(base)
	if old == base { // `.hidden` has no extension
		old = ""
	}
	return strings.TrimSuffix(file, old) + "." + ext
}

// resolve is the context to use: KUBEN_URL with KUBEN_TOKEN, else the named
// or current one.
func (c contextsDoc) resolve(name opt.Val[string], env envLookup) (serverContext, error) {
	url, hasURL := env("KUBEN_URL")
	token, hasToken := env("KUBEN_TOKEN")
	if hasURL && hasToken {
		return serverContext{URL: url, Token: token}, nil
	}
	chosen, ok := name.Get()
	if !ok {
		chosen, ok = c.Current.Get()
	}
	if !ok {
		return serverContext{}, errors.New("not logged in: kuben login https://<your server> (or set KUBEN_URL and KUBEN_TOKEN)")
	}
	found, ok := c.Contexts[chosen]
	if !ok {
		return serverContext{}, fmt.Errorf("no context `%s`; kuben login creates it", chosen)
	}
	return found, nil
}

// clientSession is a client of the chosen server, and the defaults for app
// names.
type clientSession struct {
	api     client.Client
	context serverContext
	target  clientTarget
}

func openClientSession(t clientTarget, env envLookup) (clientSession, error) {
	file, err := contextsFile(env)
	if err != nil {
		return clientSession{}, err
	}
	doc, err := loadContexts(file)
	if err != nil {
		return clientSession{}, err
	}
	chosen, err := doc.resolve(t.context, env)
	if err != nil {
		return clientSession{}, err
	}
	return clientSession{api: client.New(chosen.URL, chosen.Token), context: chosen, target: t}, nil
}

// project is the project of apps named without one: the flag's, else the
// context's.
func (s clientSession) project() opt.Val[string] {
	return orClientVal(s.target.project, s.context.Project)
}

// environment is the environment of apps named without one.
func (s clientSession) environment() opt.Val[string] {
	return orClientVal(s.target.environment, s.context.Environment)
}

func (s clientSession) app(text string) (client.AppPath, error) {
	return client.ParseAppPath(text, s.project(), s.environment()) //nolint:wrapcheck // the message is the error
}

func orClientVal[T any](first, second opt.Val[T]) opt.Val[T] {
	if first.IsSome() {
		return first
	}
	return second
}

// hostOf is the host of a server's address, which names its context.
func hostOf(url string) string {
	rest := url
	if _, after, found := strings.Cut(url, "://"); found {
		rest = after
	}
	if i := strings.IndexAny(rest, "/:"); i >= 0 {
		return rest[:i]
	}
	return rest
}
