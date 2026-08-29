package main

import (
	"os"
	"testing"
	"time"
)

func TestScreenIdleDefault(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want time.Duration
	}{
		{"", 5 * time.Minute},
		{"off", 0},
		{"never", 0},
		{"0", 0},
		{"90s", 90 * time.Second},
		{"  1h ", time.Hour},
		{"OFF", 0},
		{"garbage", 0},
	} {
		os.Setenv("SCREEN_IDLE", tc.env)
		if got := screenIdleDefault(); got != tc.want {
			t.Errorf("SCREEN_IDLE=%q: got %v, want %v", tc.env, got, tc.want)
		}
	}
	os.Unsetenv("SCREEN_IDLE")
}
