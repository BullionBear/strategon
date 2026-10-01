//go:build !linux

package artifact

func statKey(string) (rootKey, bool) { return rootKey{}, false }

func scanProcRoots() map[rootKey][]int { return nil }
