package web

import (
	"reflect"
	"testing"
)

func TestNativeDirectoryPickerCommands(t *testing.T) {
	const initial = "/workspace/current"
	tests := []struct {
		name string
		goos string
		want directoryPickerCommand
	}{
		{
			name: "macOS uses AppleScript folder chooser",
			goos: "darwin",
			want: directoryPickerCommand{
				name: "osascript",
				args: []string{
					"-e",
					`on run argv
try
set selectedFolder to choose folder with prompt "Choose a workspace folder" default location (POSIX file (item 1 of argv))
return POSIX path of selectedFolder
on error number -128
return ""
end try
end run`,
					"--",
					initial,
				},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := nativeDirectoryPickerCommand(test.goos, initial)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("command = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestNativeDirectoryPickerCommandRejectsUnsupportedPlatform(t *testing.T) {
	for _, goos := range []string{"linux", "windows", "unsupported"} {
		if _, err := nativeDirectoryPickerCommand(goos, "/workspace"); err == nil {
			t.Fatalf("unsupported platform %q was accepted", goos)
		}
	}
}
