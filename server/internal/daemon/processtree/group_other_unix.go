//go:build !windows && !darwin

package processtree

func zombieOnlyGroup(int) bool { return false }
