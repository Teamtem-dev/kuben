package apiclient

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/Teamtem-dev/kuben/internal/core/opt"
)

// AppPath is `project/environment/app`.
type AppPath struct {
	Project     string
	Environment string
	App         string
}

// isSlug reports whether s is a name the API accepts in a path: lowercase
// letters, digits and `-`.
func isSlug(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		b := s[i]
		if (b < 'a' || b > 'z') && (b < '0' || b > '9') && b != '-' {
			return false
		}
	}
	return true
}

// ParseAppPath reads `project/environment/app`, or `environment/app` and
// `app` with the defaults given.
func ParseAppPath(text string, project, environment opt.Val[string]) (AppPath, error) {
	parts := strings.Split(text, "/")
	var app string
	switch len(parts) {
	case 3:
		project, environment, app = opt.Some(parts[0]), opt.Some(parts[1]), parts[2]
	case 2:
		environment, app = opt.Some(parts[0]), parts[1]
	case 1:
		app = parts[0]
	default:
		return AppPath{}, fmt.Errorf("`%s` is not project/environment/app", text)
	}
	p, hasProject := project.Get()
	e, hasEnvironment := environment.Get()
	if !hasProject || !hasEnvironment {
		return AppPath{}, fmt.Errorf("`%s` needs its project and environment: project/environment/app, or set them with "+
			"kuben login --project <p> --environment <e>", text)
	}
	for _, part := range []string{p, e, app} {
		if !isSlug(part) {
			return AppPath{}, fmt.Errorf("`%s` is not a valid name", part)
		}
	}
	return AppPath{Project: p, Environment: e, App: app}, nil
}

// api is the app's path under /api/v1.
func (a AppPath) api() string {
	return "/projects/" + a.Project + "/environments/" + a.Environment + "/apps/" + a.App
}

// String is `project/environment/app`.
func (a AppPath) String() string {
	return a.Project + "/" + a.Environment + "/" + a.App
}

// LogOptions says which log lines to read.
type LogOptions struct {
	// Tail is the lines per pod to start with.
	Tail opt.Val[int64]
	// Process keeps only the pods of this process; a name that is not a
	// slug is left out of the query.
	Process opt.Val[string]
	// Previous reads the container before its last restart.
	Previous bool
}

// query is the query string of a logs request, with only what is asked.
func (o LogOptions) query(follow bool) string {
	var q []string
	if tail, ok := o.Tail.Get(); ok {
		q = append(q, "tail="+strconv.FormatInt(tail, 10))
	}
	if process, ok := o.Process.Get(); ok && isSlug(process) {
		q = append(q, "process="+process)
	}
	if o.Previous {
		q = append(q, "previous=true")
	}
	if follow {
		q = append(q, "follow=true")
	}
	if len(q) == 0 {
		return ""
	}
	return "?" + strings.Join(q, "&")
}
