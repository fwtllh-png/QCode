package assembly

import (
	"encoding/json"
	"errors"
)

var errDuplicateJSONMember = errors.New("tool arguments contain a duplicate JSON object member")

type jsonMemberScope struct {
	object bool
	key    bool
	names  map[string]struct{}
}

// jsonMemberTracker checks member uniqueness while arguments are still being
// generated. Each byte is visited once; only open object scopes and the current
// member name are retained. String values (including source code) are skipped.
// This tracks structure and string boundaries, not the full JSON grammar:
// json.Valid remains responsible for syntax before a call becomes executable.
type jsonMemberTracker struct {
	scopes   []jsonMemberScope
	inString bool
	escaped  bool
	isKey    bool
	name     []byte
}

func (v *jsonMemberTracker) Append(fragment string) error {
	for i := 0; i < len(fragment); i++ {
		c := fragment[i]
		if v.inString {
			if v.isKey {
				v.name = append(v.name, c)
			}
			if v.escaped {
				v.escaped = false
				continue
			}
			switch c {
			case '\\':
				v.escaped = true
			case '"':
				v.inString = false
				if v.isKey {
					var name string
					if err := json.Unmarshal(v.name, &name); err != nil {
						return errors.New("tool arguments contain an invalid JSON object member name")
					}
					scope := &v.scopes[len(v.scopes)-1]
					if _, duplicate := scope.names[name]; duplicate {
						return errDuplicateJSONMember
					}
					scope.names[name] = struct{}{}
					scope.key = false
					v.name = nil
				}
			}
			continue
		}
		switch c {
		case '{':
			v.scopes = append(v.scopes, jsonMemberScope{
				object: true, key: true, names: make(map[string]struct{}),
			})
		case '[':
			v.scopes = append(v.scopes, jsonMemberScope{})
		case '}', ']':
			if len(v.scopes) != 0 {
				last := len(v.scopes) - 1
				v.scopes[last] = jsonMemberScope{}
				v.scopes = v.scopes[:last]
			}
		case ',':
			if len(v.scopes) != 0 {
				scope := &v.scopes[len(v.scopes)-1]
				scope.key = scope.object
			}
		case '"':
			v.inString = true
			v.isKey = len(v.scopes) != 0 && v.scopes[len(v.scopes)-1].key
			if v.isKey {
				v.name = append(v.name[:0], c)
			}
		}
	}
	return nil
}
