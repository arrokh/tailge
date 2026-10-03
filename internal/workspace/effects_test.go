package workspace

import "testing"

func TestEffectForKeyKeepsExternalActionsTyped(t *testing.T) {
	tests := []struct {
		key  string
		kind EffectKind
	}{
		{"r", EffectRefresh},
		{"R", EffectRetry},
		{"o", EffectOpenObservedURL},
		{"y", EffectCopyURL},
		{"c", EffectCancelOperation},
		{"x", EffectTerminateProcess},
		{"q", EffectQuit},
		{"ctrl+c", EffectQuit},
	}
	for _, test := range tests {
		got, ok := EffectForKey(test.key)
		if !ok || got.Kind != test.kind {
			t.Errorf("EffectForKey(%q) = %#v, %t; want kind %v", test.key, got, ok, test.kind)
		}
	}
	for _, key := range []string{"s", "O"} {
		if _, ok := EffectForKey(key); ok {
			t.Fatalf("disabled/local key %q was incorrectly mapped to an external effect", key)
		}
	}
}
