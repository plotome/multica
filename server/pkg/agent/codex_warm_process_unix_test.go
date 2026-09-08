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

func TestParseCodexWarmProcessGroupMembersIgnoresZombies(t *testing.T) {
	members, err := parseCodexWarmProcessGroupMembers([]byte("101 42 S\n102 42 Z\n103 42 Z+\n104 7 R\n"), 42)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]struct{}{101: {}}
	if !reflect.DeepEqual(members, want) {
		t.Fatalf("members = %#v, want %#v", members, want)
	}
}
