package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestVersionJSONIncludesExactBuildCommit(t *testing.T) {
	previousVersion, previousCommit, previousBuild := version, commit, buildCommit
	t.Cleanup(func() { version, commit, buildCommit = previousVersion, previousCommit, previousBuild })
	version, commit, buildCommit = "0.7.1-test", "abcdef0", strings.Repeat("a", 40)
	for _, full := range []string{buildCommit, "unknown"} {
		buildCommit = full
		var output bytes.Buffer
		command := &cobra.Command{}
		command.Flags().String("output", "json", "")
		command.SetOut(&output)
		if err := runVersion(command, nil); err != nil {
			t.Fatal(err)
		}
		var value map[string]string
		if err := json.Unmarshal(output.Bytes(), &value); err != nil {
			t.Fatal(err)
		}
		if value["build_commit"] != full || value["commit"] != "abcdef0" || value["version"] != version {
			t.Fatalf("version identity was rewritten: %#v", value)
		}
	}
}
