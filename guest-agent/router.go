package main

import (
	"net/http"
	"strings"
)

// Docker API routing: a declarative table instead of a monolithic switch.
//
// A pattern is slash-separated. A ":name" segment captures exactly one path
// segment; a "*name" segment captures one or more path segments including
// slashes (image references like docker.io/library/alpine contain them).
// Routes without parameters are matched before parameterized ones, so
// /containers/json is never captured by /containers/:id.
//
// Add a new endpoint by writing a handler func(w, r, p) and appending a
// newRoute entry to the matching domain table below — nothing else.

// routeParams carries the captured path variables to a handler.
type routeParams map[string]string

// apiRoute is one Docker API endpoint. method == "" matches any method.
type apiRoute struct {
	method   string
	pattern  []string
	hasParam bool
	handler  func(w http.ResponseWriter, r *http.Request, p routeParams)
}

func newRoute(method, pattern string, handler func(w http.ResponseWriter, r *http.Request, p routeParams)) apiRoute {
	rt := apiRoute{method: method, pattern: splitPath(pattern), handler: handler}
	for _, seg := range rt.pattern {
		if strings.HasPrefix(seg, ":") || strings.HasPrefix(seg, "*") {
			rt.hasParam = true
		}
	}
	return rt
}

func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// match reports whether the path segments fit the route pattern, returning
// the captured variables (nil when there is no match).
func (rt apiRoute) match(segs []string) (routeParams, bool) {
	return matchSegments(rt.pattern, segs)
}

// matchSegments matches pattern segments against path segments. ":name"
// captures one segment; "*name" captures one or more segments (slashes
// included — image references contain them) and may be followed by literal
// suffix segments, resolved by backtracking.
func matchSegments(pattern, segs []string) (routeParams, bool) {
	if len(pattern) == 0 {
		return routeParams{}, len(segs) == 0
	}
	pat := pattern[0]
	switch {
	case strings.HasPrefix(pat, "*"):
		for take := 1; take <= len(segs); take++ {
			if rest, ok := matchSegments(pattern[1:], segs[take:]); ok {
				rest[pat[1:]] = strings.Join(segs[:take], "/")
				return rest, true
			}
		}
		return nil, false
	case strings.HasPrefix(pat, ":"):
		if len(segs) == 0 || segs[0] == "" {
			return nil, false
		}
		rest, ok := matchSegments(pattern[1:], segs[1:])
		if !ok {
			return nil, false
		}
		rest[pat[1:]] = segs[0]
		return rest, true
	default:
		if len(segs) == 0 || segs[0] != pat {
			return nil, false
		}
		return matchSegments(pattern[1:], segs[1:])
	}
}

// dockerRoutes is the full endpoint table, assembled from the per-domain
// tables so each Docker API area stays in its own file.
var dockerRoutes = concatRoutes(systemRoutes, containerRoutes, imageRoutes, networkRoutes, volumeRoutes)

func concatRoutes(groups ...[]apiRoute) []apiRoute {
	var all []apiRoute
	for _, g := range groups {
		all = append(all, g...)
	}
	return all
}

// dispatchDockerAPI routes a version-stripped API path. Literal routes are
// tried first, then parameterized ones; an unrouted path is a 404 (a routed
// path with the wrong method too, matching the old switch behavior).
func dispatchDockerAPI(w http.ResponseWriter, r *http.Request, path string) {
	segs := splitPath(path)
	for _, paramPass := range []bool{false, true} {
		for _, rt := range dockerRoutes {
			if rt.method != "" && rt.method != r.Method {
				continue
			}
			if rt.hasParam != paramPass {
				continue
			}
			if p, ok := rt.match(segs); ok {
				rt.handler(w, r, p)
				return
			}
		}
	}
	http.NotFound(w, r)
}
