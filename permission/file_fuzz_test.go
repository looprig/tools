package permission

import (
	"bytes"
	"reflect"
	"testing"
)

// FuzzPermissionFile fuzzes the strict schema-v2 codec: decoding arbitrary
// bytes must never panic, every accepted document must survive an
// encode/decode round trip unchanged, and every accepted rule must be
// individually valid (so no fuzz-found input can smuggle an invalid or
// foreign-field record past the loader).
func FuzzPermissionFile(f *testing.F) {
	seeds, err := encodeFile(sampleRules())
	if err != nil {
		f.Fatalf("seed encode: %v", err)
	}
	f.Add(seeds)
	f.Add([]byte(`{"version":2,"normalization_version":1,"rules":[]}`))
	f.Add([]byte(`{"version":2,"normalization_version":1,"rules":[{"effect":"deny","capability":"network","enforcement_class":"network.target.v1","match":{"host":"github.com"}}]}`))
	f.Add([]byte(`{"version":1,"rules":{}}`))
	f.Add([]byte(`Bash(git log:*)`))

	f.Fuzz(func(t *testing.T, data []byte) {
		rules, err := decodeFile(data)
		if err != nil {
			return
		}
		for index, rule := range rules {
			if err := rule.validate(index); err != nil {
				t.Fatalf("decoder accepted invalid rule %d: %v (%#v)", index, err, rule)
			}
			if rule.carriesForeignFields() {
				t.Fatalf("decoder accepted rule %d carrying foreign fields: %#v", index, rule)
			}
		}
		encoded, err := encodeFile(rules)
		if err != nil {
			t.Fatalf("accepted rules failed to re-encode: %v", err)
		}
		again, err := decodeFile(encoded)
		if err != nil {
			t.Fatalf("re-encoded file failed to decode: %v\n%s", err, encoded)
		}
		if len(rules) == 0 && len(again) == 0 {
			return
		}
		if !reflect.DeepEqual(rules, again) {
			t.Fatalf("round trip mismatch:\n got %#v\nwant %#v", again, rules)
		}
		second, err := encodeFile(again)
		if err != nil || !bytes.Equal(encoded, second) {
			t.Fatalf("encoding is not deterministic (%v)", err)
		}
	})
}
