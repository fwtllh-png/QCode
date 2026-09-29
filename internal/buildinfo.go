package buildinfo

import (
	"runtime"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

type Info struct {
	Name                   string `json:"name"`
	Version                string `json:"version"`
	Commit                 string `json:"commit"`
	BuildDate              string `json:"build_date"`
	GoVersion              string `json:"go_version"`
	OS                     string `json:"os"`
	Arch                   string `json:"arch"`
	WebProtocolVersion     int    `json:"web_protocol_version"`
	OperationSchemaVersion int    `json:"operation_schema_version"`
}

func Current() Info {
	return Info{
		Name:                   "qcode",
		Version:                Version,
		Commit:                 Commit,
		BuildDate:              Date,
		GoVersion:              runtime.Version(),
		OS:                     runtime.GOOS,
		Arch:                   runtime.GOARCH,
		WebProtocolVersion:     1,
		OperationSchemaVersion: protocol.Version,
	}
}
