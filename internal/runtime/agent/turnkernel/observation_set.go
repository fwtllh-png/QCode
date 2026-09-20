package turnkernel

import "encoding/json"

// ObservationSet is the persisted seen-observation ledger. It keeps
// membership checks O(1) while marshaling exactly like the historical
// []string encoding (insertion order preserved); that byte layout is a
// storage contract, because every state digest in the domain-fact journal
// depends on it. A set is nil unless it holds at least one key, so the
// omitempty field keeps omitting empty ledgers.
type ObservationSet struct {
	order []string
	index map[string]struct{}
}

func (o *ObservationSet) contains(key string) bool {
	if o == nil {
		return false
	}
	_, seen := o.index[key]
	return seen
}

// add inserts key unless present. Sets are only mutated on transition copies
// that cloneProgressState already isolated.
func (o *ObservationSet) add(key string) {
	if o == nil || o.contains(key) {
		return
	}
	o.order = append(o.order, key)
	if o.index == nil {
		o.index = make(map[string]struct{}, 1)
	}
	o.index[key] = struct{}{}
}

func (o ObservationSet) clone() ObservationSet {
	cloned := ObservationSet{order: append([]string(nil), o.order...)}
	if o.index != nil {
		cloned.index = make(map[string]struct{}, len(o.index))
		for key := range o.index {
			cloned.index[key] = struct{}{}
		}
	}
	return cloned
}

func (o ObservationSet) MarshalJSON() ([]byte, error) {
	return json.Marshal(o.order)
}

func (o *ObservationSet) UnmarshalJSON(encoded []byte) error {
	var keys []string
	if err := json.Unmarshal(encoded, &keys); err != nil {
		return err
	}
	if len(keys) == 0 {
		o.order, o.index = nil, nil
		return nil
	}
	o.order = keys
	o.index = make(map[string]struct{}, len(keys))
	for _, key := range keys {
		o.index[key] = struct{}{}
	}
	return nil
}
