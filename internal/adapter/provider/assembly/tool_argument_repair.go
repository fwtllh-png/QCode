package assembly

import (
	"errors"
	"fmt"
)

var ErrToolArgumentRepairLimit = errors.New("tool argument regeneration budget exhausted")

func (a *ResponseAssembly) ToolArgumentsRejected() bool {
	segment := a.currentOrNil()
	return segment != nil && segment.State == ResponseFailed && segment.RejectedToolArguments
}

// AuthorizeToolArgumentRepair records a new response attempt without changing
// the rejected segment. Reapplying the same authorization after restart is
// idempotent; failed output and all observed usage remain available for audit.
func (a *ResponseAssembly) AuthorizeToolArgumentRepair(limit int) error {
	if !a.ToolArgumentsRejected() {
		return errors.New("response has no rejected tool arguments")
	}
	if a.toolArgumentRepairAuthorized() {
		return nil
	}
	if len(a.ToolArgumentRepairs) >= max(limit, 0) {
		return ErrToolArgumentRepairLimit
	}
	a.ToolArgumentRepairs = append(a.ToolArgumentRepairs, uint32(len(a.Segments)))
	return nil
}

func (a *ResponseAssembly) toolArgumentRepairAuthorized() bool {
	return a.ToolArgumentsRejected() && len(a.ToolArgumentRepairs) != 0 &&
		a.ToolArgumentRepairs[len(a.ToolArgumentRepairs)-1] == uint32(len(a.Segments))
}

func (a *ResponseAssembly) validateToolArgumentRepairs() error {
	var previous uint32
	for _, index := range a.ToolArgumentRepairs {
		if index <= previous || uint64(index) > uint64(len(a.Segments)) || !a.Segments[index-1].RejectedToolArguments {
			return errors.New("invalid tool argument repair authorization")
		}
		previous = index
	}
	for i, segment := range a.Segments[:len(a.Segments)-1] {
		if segment.State != ResponseFailed {
			continue
		}
		authorized := false
		for _, index := range a.ToolArgumentRepairs {
			if index == uint32(i+1) {
				authorized = true
				break
			}
		}
		if !authorized {
			return fmt.Errorf("failed response segment %d was resumed without authorization", i)
		}
	}
	return nil
}
