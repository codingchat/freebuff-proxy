package convert

import (
	"encoding/json"
	"reflect"
	"testing"
)

// mustJSON marshals v, failing the test on error.
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %#v: %v", v, err)
	}
	return b
}

// decode parses JSON bytes into a map, failing the test on error.
func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal %q: %v", b, err)
	}
	return m
}

// assertJSONEq asserts got (JSON bytes) equals want (Go value) after both
// are normalized through encoding/json.
func assertJSONEq(t *testing.T, got []byte, want any) {
	t.Helper()
	wantBytes := mustJSON(t, want)
	var gotV, wantV any
	if err := json.Unmarshal(got, &gotV); err != nil {
		t.Fatalf("got is not JSON: %v\n%s", err, got)
	}
	if err := json.Unmarshal(wantBytes, &wantV); err != nil {
		t.Fatalf("want is not JSON: %v", err)
	}
	if !reflect.DeepEqual(gotV, wantV) {
		t.Fatalf("mismatch:\n got: %s\nwant: %s", got, wantBytes)
	}
}
