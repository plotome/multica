//go:build !windows && !darwin

package processtree

func groupHasNoLiveMembers(int) bool { return false }
