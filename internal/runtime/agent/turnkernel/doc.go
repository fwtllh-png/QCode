// Package turnkernel owns authoritative turn state, legal transitions, durable
// domain facts and effect identities. Coordinators commit facts before exposing
// state or dispatching effects. Execution and tool concurrency belong to Engine;
// concrete persistence implementations are injected through the store contracts.
package turnkernel
