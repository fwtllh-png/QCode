//go:build ignore

// The browser fixture launches a real Runtime; check its OS sandbox capability
// before starting the suite so an unavailable host produces one useful failure.
package main

import (
	"fmt"
	"os"

	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func main() {
	capability := sandbox.Probe()
	if !capability.Available || sandbox.DefaultProcessRequirements().SatisfiedBy(capability.Effective) != nil {
		fmt.Fprintf(os.Stderr, "sandbox_unavailable: browser fixtures require a working OS sandbox: %+v\nRequest exec_command with execution_target=host (Auto asks for one-time approval; Full Access preauthorizes it), or run from a terminal outside an enclosing sandbox. Default commands still use the OS sandbox. No tests were executed.\n", capability)
		os.Exit(1)
	}
	fmt.Printf("Browser fixture sandbox available: %s\n", capability.Backend)
}
