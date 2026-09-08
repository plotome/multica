//go:build !windows

package agent

import (
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func codexWarmTestProcessGone(pid int) bool {
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	return err != nil || strings.HasPrefix(strings.ToUpper(strings.TrimSpace(string(out))), "Z")
}

func TestParseCodexWarmOwnedProcessesIncludesDetachedDescendantsAndIgnoresZombies(t *testing.T) {
	members, err := parseCodexWarmOwnedProcesses([]byte(""+
		"42 1 42 S\n"+
		"101 42 42 S\n"+
		"102 42 102 S\n"+ // setsid/new process group, still a descendant
		"103 102 103 R\n"+
		"104 42 42 Z\n"+
		"105 42 42 Z+\n"+
		"106 1 7 R\n"), 42)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]struct{}{42: {}, 101: {}, 102: {}, 103: {}}
	if !reflect.DeepEqual(members, want) {
		t.Fatalf("members = %#v, want %#v", members, want)
	}
}
