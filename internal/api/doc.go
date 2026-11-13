// Package api is the HTTP boundary: it decodes requests, maps them onto the
// hub's domain operations and renders responses. It holds no SQL and no domain
// rules, and its presenters are the only place a response body is built.
package api
