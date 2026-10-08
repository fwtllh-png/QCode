package tool

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
)

type resultCacheEntry struct {
	callID string
	result Result
}

type ResultCache struct {
	revision                    uint64
	entries                     map[string]resultCacheEntry
	suppressedNonRetryableCalls int
}

type ReplayPlan struct {
	Results          []Result
	SkipExecution    []bool
	AlreadyExecuted  []bool
	Fingerprints     []string
	CacheSources     []string
	ParallelPolicies []ParallelPolicy
	ReplaySuccessful []bool
	DuplicateOwners  map[int]int
}

func (c *ResultCache) Plan(
	calls []provider.ToolCall,
	executed map[string]Result,
	registry *Registry,
) ReplayPlan {
	plan := ReplayPlan{
		Results:          make([]Result, len(calls)),
		SkipExecution:    make([]bool, len(calls)),
		AlreadyExecuted:  make([]bool, len(calls)),
		Fingerprints:     make([]string, len(calls)),
		CacheSources:     make([]string, len(calls)),
		ParallelPolicies: make([]ParallelPolicy, len(calls)),
		ReplaySuccessful: make([]bool, len(calls)),
		DuplicateOwners:  make(map[int]int),
	}
	for index := range plan.ParallelPolicies {
		plan.ParallelPolicies[index] = ParallelSerial
	}
	if c.entries == nil {
		c.entries = make(map[string]resultCacheEntry)
	}
	accessModes := make([]AccessMode, len(calls))
	writeCalls := 0
	for index, call := range calls {
		binding := BindingForCall(call)
		_, descriptor, _, err := registry.ResolveBound(call.Name, binding)
		if err != nil {
			continue
		}
		accessModes[index] = descriptor.AccessMode
		if descriptor.AccessMode == AccessWrite {
			writeCalls++
		}
	}
	batchOwners := make(map[string]int)
	for index, call := range calls {
		if previous, exists := executed[call.ID]; exists {
			plan.Results[index] = previous
			plan.SkipExecution[index] = true
			plan.AlreadyExecuted[index] = true
			continue
		}
		binding := BindingForCall(call)
		_, descriptor, _, err := registry.ResolveBound(call.Name, binding)
		if err != nil {
			continue
		}
		plan.ParallelPolicies[index] = descriptor.ParallelPolicy
		plan.ReplaySuccessful[index] =
			descriptor.RepeatPolicy == RepeatReplaySameTurn
		fingerprint, err := resultFingerprint(call, binding, c.revision)
		if err != nil {
			continue
		}
		plan.Fingerprints[index] = fingerprint
		if cached, exists := c.entries[fingerprint]; exists {
			otherWriteCall := writeCalls > 0
			if accessModes[index] == AccessWrite {
				otherWriteCall = writeCalls > 1
			}
			if (plan.ReplaySuccessful[index] ||
				nonRetryableResult(cached.result)) && !otherWriteCall {
				plan.Results[index] = CachedResult(cached.result, cached.callID)
				plan.CacheSources[index] = cached.callID
				plan.SkipExecution[index] = true
				if nonRetryableResult(cached.result) {
					c.suppressedNonRetryableCalls++
				}
				continue
			}
		}
		otherWriteCall := writeCalls > 0
		if accessModes[index] == AccessWrite {
			otherWriteCall = writeCalls > 1
		}
		if descriptor.RepeatPolicy != RepeatReplaySameTurn || otherWriteCall {
			continue
		}
		if owner, exists := batchOwners[fingerprint]; exists {
			plan.DuplicateOwners[index] = owner
			plan.CacheSources[index] = calls[owner].ID
			plan.SkipExecution[index] = true
			continue
		}
		batchOwners[fingerprint] = index
	}
	return plan
}

func (c *ResultCache) Commit(
	calls []provider.ToolCall,
	plan ReplayPlan,
	results []Result,
	mutated bool,
) {
	if mutated {
		c.revision++
		clear(c.entries)
		return
	}
	for index, fingerprint := range plan.Fingerprints {
		if changed, _ := results[index].Metadata["plan_delta"].(bool); changed {
			c.revision++
			clear(c.entries)
			return
		}
		if fingerprint == "" || plan.CacheSources[index] != "" ||
			!plan.ReplaySuccessful[index] &&
				!nonRetryableResult(results[index]) {
			continue
		}
		c.entries[fingerprint] = resultCacheEntry{
			callID: calls[index].ID,
			result: results[index],
		}
	}
}

func (c *ResultCache) SuppressedNonRetryableCalls() int {
	if c == nil {
		return 0
	}
	return c.suppressedNonRetryableCalls
}

func nonRetryableResult(result Result) bool {
	retry, exists := result.Metadata["retry_original"].(bool)
	return result.IsError && exists && !retry
}

func BindingForCall(call provider.ToolCall) CatalogBinding {
	return CatalogBinding{
		CatalogID: call.CatalogID, Generation: call.CatalogGeneration,
		Revision: call.CatalogRevision, Authority: call.CatalogAuthority,
	}
}

func CachedResult(result Result, sourceCallID string) Result {
	copy := result
	if result.IsError {
		copy.Content = fmt.Sprintf("Cached failure from call %s; no new execution was performed.\n%s", sourceCallID, result.Content)
	}
	copy.Metadata = maps.Clone(result.Metadata)
	copy.Outcome = CloneOutcome(result.Outcome)
	copy.Execution = CloneExecutionReceipt(result.Execution)
	if copy.Metadata == nil {
		copy.Metadata = make(map[string]any)
	}
	copy.Metadata["replayed_from_call_id"] = sourceCallID
	return copy
}

func resultFingerprint(
	call provider.ToolCall,
	binding CatalogBinding,
	revision uint64,
) (string, error) {
	decoder := json.NewDecoder(bytes.NewBufferString(call.Arguments))
	decoder.UseNumber()
	var arguments any
	if err := decoder.Decode(&arguments); err != nil {
		return "", err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return "", errors.New("tool arguments contain multiple JSON values")
		}
		return "", err
	}
	canonical, err := json.Marshal(arguments)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(
		"%s\x00%s\x00%d\x00%d\x00%d\x00%d\x00%s",
		call.Name,
		binding.CatalogID,
		binding.Generation,
		binding.Revision,
		revision,
		binding.Authority,
		canonical,
	), nil
}
