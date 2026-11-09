package main

import "testing"

func TestStartupFlags(t *testing.T) {
	for _, tc := range []struct {
		name   string
		env    map[string]string
		bad    bool
		secure bool
	}{
		{"defaults", nil, false, true},
		{"local public", map[string]string{"GO_PERSISTED_RUNS": "true", "GO_PUBLIC_RUNS": "true", "PUBLIC_COOKIE_SECURE": "false"}, false, false},
		{"public without persistence", map[string]string{"GO_PUBLIC_RUNS": "true"}, true, true},
		{"unsafe typo", map[string]string{"PUBLIC_COOKIE_SECURE": "TRUE"}, true, false},
		{"silent fallback typo", map[string]string{"GO_PERSISTED_RUNS": "treu"}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := readStartupFlags(func(k string) string { return tc.env[k] })
			if (err != nil) != tc.bad {
				t.Fatal(err)
			}
			if err == nil && f.secure != tc.secure {
				t.Fatal("incorrect cookie default")
			}
		})
	}
}
