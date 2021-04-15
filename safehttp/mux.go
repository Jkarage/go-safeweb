// Copyright 2020 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// 	https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package safehttp

import (
	"fmt"
	"log"
	"net/http"
)

// The HTTP request methods defined by RFC.
const (
	MethodConnect = "CONNECT" // RFC 7231, 4.3.6
	MethodDelete  = "DELETE"  // RFC 7231, 4.3.5
	MethodGet     = "GET"     // RFC 7231, 4.3.1
	MethodHead    = "HEAD"    // RFC 7231, 4.3.2
	MethodOptions = "OPTIONS" // RFC 7231, 4.3.7
	MethodPatch   = "PATCH"   // RFC 5789
	MethodPost    = "POST"    // RFC 7231, 4.3.3
	MethodPut     = "PUT"     // RFC 7231, 4.3.4
	MethodTrace   = "TRACE"   // RFC 7231, 4.3.8
)

// ServeMux is an HTTP request multiplexer. It matches the URL of each incoming
// request against a list of registered patterns and calls the handler for
// the pattern that most closely matches the URL.
//
// Patterns names are fixed, rooted paths, like "/favicon.ico", or rooted
// subtrees like "/images/" (note the trailing slash). Longer patterns take
// precedence over shorter ones, so that if there are handlers registered for
// both "/images/" and "/images/thumbnails/", the latter handler will be called
// for paths beginning "/images/thumbnails/" and the former will receive
// requests for any other paths in the "/images/" subtree.
//
// Note that since a pattern ending in a slash names a rooted subtree, the
// pattern "/" matches all paths not matched by other registered patterns,
// not just the URL with Path == "/".
//
// If a subtree has been registered and a request is received naming the subtree
// root without its trailing slash, ServeMux redirects that request to
// the subtree root (adding the trailing slash). This behavior can be overridden
// with a separate registration for the path without the trailing slash. For
// example, registering "/images/" causes ServeMux to redirect a request for
// "/images" to "/images/", unless "/images" has been registered separately.
//
// Patterns may optionally begin with a host name, restricting matches to URLs
// on that host only.  Host-specific patterns take precedence over general
// patterns, so that a handler might register for the two patterns "/codesearch"
// and "codesearch.google.com/" without also taking over requests for
// "http://www.google.com/".
//
// ServeMux also takes care of sanitizing the URL request path and the Host
// header, stripping the port number and redirecting any request containing . or
// .. elements or repeated slashes to an equivalent, cleaner URL.
//
// Multiple handlers can be registered for a single pattern, as long as they
// handle different HTTP methods.
type ServeMux struct {
	mux *http.ServeMux
}

func registerHandlers(mux *http.ServeMux, handlers map[string]map[string]*handlerConfig) {
	for pattern, methodToHandlerConfigMap := range handlers {
		httpHandler := generateHttpHandlerFunc(methodToHandlerConfigMap)
		mux.HandleFunc(pattern, httpHandler)
	}
}

// generateHttpHandlerFunc converts a handlerConfig to a classic http.HandlerFunc.
// We need to have this map because http.Handle handles requests for any HTTP request method per default,
// which we want to avoid in safehttp.
func generateHttpHandlerFunc(methodToHandlerConfig map[string]*handlerConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg, ok := methodToHandlerConfig[r.Method]
		if !ok {
			http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
			return
		}
		processRequest(cfg, w, r)
	}
}

// HttpHandlerForTransition generates a classic http.Handler using its ServeMuxConfig
// (i.e. already installed Interceptors etc).
// This is intended to be used during transition from using the http package to safehttp.
func (s *ServeMuxConfig) HttpHandlerForTransition(method string, handler Handler, cfgs ...InterceptorConfig) http.Handler {
	cfg := s.handlerRegistrationToHandlerConfig(&handlerRegistration{
		pattern: "",
		method:  method,
		handler: handler,
		cfgs:    cfgs,
	})
	return generateHttpHandlerFunc(map[string]*handlerConfig{
		method: cfg,
	})
}

// ServeHTTP dispatches the request to the handler whose method matches the
// incoming request and whose pattern most closely matches the request URL.
//
//  For each incoming request:
//  - [Before Phase] Interceptor.Before methods are called for every installed
//    interceptor, until an interceptor writes to a ResponseWriter (including
//    errors) or panics,
//  - the handler is called after a [Before Phase] if no writes or panics occured,
//  - the handler triggers the [Commit Phase] by writing to the ResponseWriter,
//  - [Commit Phase] Interceptor.Commit methods run for every interceptor whose
//    Before method was called,
//  - [Dispatcher Phase] after the [Commit Phase], the Dispatcher's appropriate
//    write method is called; the Dispatcher is responsible for determining whether
//    the response is indeed safe and writing it,
//  - if the handler attempts to write more than once, it is treated as an
//    unrecoverable error; the request processing ends abrubptly with a panic and
//    nothing else happens (note: this will change as soon as [After Phase] is
//    introduced)
//
// Interceptors should NOT rely on the order they're run.
func (m *ServeMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mux.ServeHTTP(w, r)
}

// ServeMuxConfig is a builder for ServeMux.
type ServeMuxConfig struct {
	dispatcher   Dispatcher
	handlers     []handlerRegistration
	interceptors []Interceptor
}

// NewServeMuxConfig crates a ServeMuxConfig with the provided Dispatcher. If
// the provided Dispatcher is nil, the DefaultDispatcher is used.
func NewServeMuxConfig(disp Dispatcher) *ServeMuxConfig {
	if disp == nil {
		disp = &DefaultDispatcher{}
	}
	return &ServeMuxConfig{dispatcher: disp}
}

type handlerRegistration struct {
	pattern string
	method  string
	handler Handler
	cfgs    []InterceptorConfig
}

// Handle registers a handler for the given pattern and method. If a handler is
// registered twice for the same pattern and method, Build will panic.
//
// InterceptorConfigs can be passed in order to modify the behavior of the
// interceptors on a registered handler. Passing an InterceptorConfig whose
// corresponding Interceptor was not installed will produce no effect. If
// multiple configurations are passed for the same Interceptor, only the first
// one will take effect.
func (s *ServeMuxConfig) Handle(pattern string, method string, h Handler, cfgs ...InterceptorConfig) {
	// check for duplicate handlers
	for _, hr := range s.handlers {
		if hr.pattern == pattern && hr.method == method {
			panic(fmt.Sprintf("double registration of (pattern = %q, method = %q)", hr.pattern, hr.method))
		}
	}
	s.handlers = append(s.handlers, handlerRegistration{
		pattern: pattern,
		method:  method,
		handler: h,
		cfgs:    cfgs,
	})
}

// Intercept installs an Interceptor.
//
// Interceptors order is undetermined and should not be relied on.
func (s *ServeMuxConfig) Intercept(i Interceptor) {
	s.interceptors = append(s.interceptors, i)
}

// Mux returns the ServeMux with a copy of the current configuration.
func (s *ServeMuxConfig) Mux() *ServeMux {
	freezeLocalDev = true
	if IsLocalDev() {
		log.Println("Warning: creating safehttp.Mux in dev mode. This configuration is not valid for production use")
	}

	if s.dispatcher == nil {
		panic("Use NewServeMuxConfig instead of creating ServeMuxConfig using a composite literal.")
	}
	// pattern -> method -> handlerConfig
	handlers := make(map[string]map[string]*handlerConfig)
	for _, hr := range s.handlers {
		if handlers[hr.pattern] == nil {
			handlers[hr.pattern] = make(map[string]*handlerConfig)
		}
		handlers[hr.pattern][hr.method] = s.handlerRegistrationToHandlerConfig(&hr)
	}
	m := http.NewServeMux()
	registerHandlers(m, handlers)
	return &ServeMux{mux: m}
}

func (s *ServeMuxConfig) handlerRegistrationToHandlerConfig(hr *handlerRegistration) *handlerConfig {
	return &handlerConfig{
		Dispatcher:   s.dispatcher,
		Handler:      hr.handler,
		Interceptors: configureInterceptors(s.interceptors, hr.cfgs),
	}
}

func configureInterceptors(interceptors []Interceptor, cfgs []InterceptorConfig) []configuredInterceptor {
	var its []configuredInterceptor
	for _, it := range interceptors {
		var cfg InterceptorConfig
		for _, c := range cfgs {
			if c.Match(it) {
				// TODO: there should be a validation check that there is at
				// most one config per interceptor.
				cfg = c
				break
			}
		}
		its = append(its, configuredInterceptor{interceptor: it, config: cfg})
	}
	return its
}

// Clone creates a copy of the current config.
// This can be used to create several instances of Mux that share the same set of
// plugins and some common handlers.
func (s *ServeMuxConfig) Clone() *ServeMuxConfig {
	c := &ServeMuxConfig{
		dispatcher:   s.dispatcher,
		handlers:     make([]handlerRegistration, len(s.handlers)),
		interceptors: make([]Interceptor, len(s.interceptors)),
	}
	copy(c.handlers, s.handlers)
	copy(c.interceptors, s.interceptors)
	return c
}
