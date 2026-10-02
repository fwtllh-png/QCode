// Package contextview selects bounded model input from context facts. It owns
// visible history, economic admission, stateless projection and cache-prefix
// identities. It does not mutate durable context or execute providers and tools;
// Engine owns when projections run and retains per-turn folding state.
package contextview
