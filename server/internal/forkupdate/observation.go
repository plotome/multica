package forkupdate

import (
	"errors"
	"time"
)

type InstalledComponent struct {
	Kind    string `json:"kind"`
	Version string `json:"version"`
	Commit  string `json:"commit"`
}
type Observation struct {
	At         string               `json:"at"`
	Components []InstalledComponent `json:"components"`
}

func ExpectedComponents(kind string) []string {
	switch kind {
	case "desktop":
		return []string{"desktop", "cli", "daemon"}
	case "server-web":
		return []string{"server", "web"}
	case "cli-daemon":
		return []string{"cli", "daemon"}
	default:
		return nil
	}
}

func (o Observation) Validate(kind, commit string) error {
	if _, err := time.Parse(time.RFC3339Nano, o.At); err != nil {
		return errors.New("missing observation time")
	}
	expected := ExpectedComponents(kind)
	if len(expected) == 0 || len(expected) != len(o.Components) {
		return errors.New("running component inventory mismatch")
	}
	seen := map[string]bool{}
	for _, c := range o.Components {
		if c.Version == "" || c.Commit != commit || seen[c.Kind] {
			return errors.New("running component identity mismatch")
		}
		seen[c.Kind] = true
	}
	for _, k := range expected {
		if !seen[k] {
			return errors.New("running component missing")
		}
	}
	return nil
}
