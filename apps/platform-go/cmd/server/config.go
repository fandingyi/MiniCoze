package main

import "fmt"

type startupFlags struct{ persisted, public, secure bool }

func readStartupFlags(get func(string) string) (startupFlags, error) {
	var flags startupFlags
	for _, item := range []struct {
		name     string
		target   *bool
		fallback bool
	}{
		{"GO_PERSISTED_RUNS", &flags.persisted, false},
		{"GO_PUBLIC_RUNS", &flags.public, false},
		{"PUBLIC_COOKIE_SECURE", &flags.secure, true},
	} {
		switch get(item.name) {
		case "":
			*item.target = item.fallback
		case "true":
			*item.target = true
		case "false":
			*item.target = false
		default:
			return flags, fmt.Errorf("%s must be true or false", item.name)
		}
	}
	if flags.public && !flags.persisted {
		return flags, fmt.Errorf("GO_PUBLIC_RUNS requires GO_PERSISTED_RUNS=true")
	}
	return flags, nil
}
