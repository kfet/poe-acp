// Package router: defensive helper for the convo Manager constructor.
// Excluded from coverage via the `_must.go` suffix rule in .covignore.
package router

import "github.com/kfet/acp-kit/convo"

// mustConvo panics if convo.New failed. The router passes no override
// Store and always an Agent (New validates it first), and those are the
// only two ways convo.New can fail — so an error here is a programming
// error in the wiring, not a runtime condition.
func mustConvo(m *convo.Manager, err error) *convo.Manager {
	if err != nil {
		panic("router: convo manager: " + err.Error())
	}
	return m
}
