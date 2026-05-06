// Package engine contains the workflow state machine, scheduler loop, and
// the race-and-cancel booking orchestration. It depends on the providers
// interface — never on a concrete adapter — so a second provider can be
// added without engine changes.
package engine
