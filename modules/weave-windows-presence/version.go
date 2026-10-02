//go:build windows

package main

import "fmt"

const currentVersionKey = `SOFTWARE\Microsoft\Windows NT\CurrentVersion`

// windowsVersion reads the version Win32_OperatingSystem reports
// ("10.0.22631") and its build ("22631") from the registry values WMI itself
// derives them from. The registry is used rather than WMI because it needs no
// COM apartment and answers in microseconds, which matters for hello: it is
// the first thing a host sends, and it is sent before authentication.
//
// CurrentMajorVersionNumber and CurrentMinorVersionNumber exist from Windows 10
// and Server 2016 on; an older system reports its build and no version.
func windowsVersion(
	str func(path, name string) string,
	dword func(path, name string) (uint32, bool),
) (version, build string) {
	build = str(currentVersionKey, "CurrentBuildNumber")
	major, okMajor := dword(currentVersionKey, "CurrentMajorVersionNumber")
	minor, okMinor := dword(currentVersionKey, "CurrentMinorVersionNumber")
	if build == "" || !okMajor || !okMinor {
		return "", build
	}
	return fmt.Sprintf("%d.%d.%s", major, minor, build), build
}
