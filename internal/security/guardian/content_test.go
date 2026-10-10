package guardian

import (
	"slices"
	"testing"
)

func TestContentCoverageDiscoversExecutionDependencies(t *testing.T) {
	bodies := map[string][]byte{
		"scripts/build.sh":    []byte("#!/bin/sh\n./scripts/generate.sh && printf done > generated/status\n"),
		"scripts/generate.sh": []byte("#!/bin/sh\nprintf 'reviewed\\n' > generated/out.txt\n"),
	}
	got := AnalyzeContent("./scripts/build.sh", ".", nil)
	if !slices.Equal(got.Required, []string{"scripts/build.sh"}) || len(got.Missing) == 0 {
		t.Fatalf("undiscovered root script: %+v", got)
	}
	got = AnalyzeContent("./scripts/build.sh", ".", bodies)
	if !slices.Equal(got.Required, []string{"scripts/build.sh", "scripts/generate.sh"}) || len(got.Missing) != 0 {
		t.Fatalf("nested dependencies: %+v", got)
	}
	got = AnalyzeContent("./generate.sh", "scripts", bodies)
	if !slices.Equal(got.Required, []string{"scripts/generate.sh"}) || len(got.Missing) != 0 {
		t.Fatalf("cwd resolution: %+v", got)
	}
}

func TestContentCoverageKeepsUnknownDependenciesIncomplete(t *testing.T) {
	for _, command := range []string{
		"go test ./...", "npm test", "python main.py", "sh ./build.sh",
		"./build.sh $MODE", "./*.sh", "MODE=test ./build.sh",
		"eval 'printf done'", "printf '%s' $(./build.sh)", "printf '%s' ${VALUE}",
		"if true; then ./build.sh; fi", "(./build.sh)", "{ ./build.sh; }",
		". ./build.sh", "./build.sh < input", "./build.sh 2>&1", "printf done > /tmp/out",
		"printf done > ../out", "./../../build.sh", "printf '\\n", "cat <<EOF\ncode\nEOF",
		"./link/../build.sh", "printf changed > link/../build.sh; ./build.sh",
	} {
		t.Run(command, func(t *testing.T) {
			coverage := AnalyzeContent(command, ".", map[string][]byte{"build.sh": []byte("#!/bin/sh\nprintf done\n")})
			if len(coverage.Missing) == 0 {
				t.Fatalf("unknown execution declared complete: %+v", coverage)
			}
		})
	}
}

func TestContentCoverageRejectsMutableOrUnknownScriptContent(t *testing.T) {
	for _, body := range []string{
		"#!/bin/bash\nprintf done\n", "#!/usr/bin/env sh\nprintf done\n",
		"printf done\n", "#!/bin/sh\n./build.sh\n", "#!/bin/sh\npython generate.py\n",
		"#!/bin/sh\nprintf changed > build.sh\n", "#!/bin/sh\nprintf '\xff'\n",
	} {
		t.Run(body, func(t *testing.T) {
			if got := AnalyzeContent("./build.sh", ".", map[string][]byte{"build.sh": []byte(body)}); len(got.Missing) == 0 {
				t.Fatalf("unsupported script declared complete: %+v", got)
			}
		})
	}
	for _, command := range []string{"printf changed > build.sh; ./build.sh", "./build.sh > build.sh", "./build.sh >> ./build.sh"} {
		if got := AnalyzeContent(command, ".", map[string][]byte{"build.sh": []byte("#!/bin/sh\nprintf done\n")}); len(got.Missing) == 0 {
			t.Fatalf("self modification declared complete: %s: %+v", command, got)
		}
	}
}
