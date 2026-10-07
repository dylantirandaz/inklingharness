package main

import "testing"

// A mistyped INKLING_MOTION must fail instead of choosing a mode silently.
func TestMotionSetting(t *testing.T) {
	for value, want := range map[string]bool{"": true, "on": true, "off": false} {
		if got, err := motionSetting(value); err != nil || got != want {
			t.Fatalf("motionSetting(%q) = %t, %v", value, got, err)
		}
	}
	for _, value := range []string{"0", "OFF", "false", "reduced"} {
		if _, err := motionSetting(value); err == nil {
			t.Fatalf("motionSetting(%q) accepted an unknown value", value)
		}
	}
}
