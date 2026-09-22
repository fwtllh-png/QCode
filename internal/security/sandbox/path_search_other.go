//go:build !darwin

package sandbox

func extraPlatformPATHDirectories() []string { return nil }
