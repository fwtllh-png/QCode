package turnkernel

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
)

func FormatWorkspaceContent(item WorkItem) string {
	parts := make([]string, 0, len(item.KnownReads)+len(item.KnownEdits))
	readKeys := make([]string, 0, len(item.KnownReads))
	for path := range item.KnownReads {
		if strings.TrimSpace(path) != "" {
			readKeys = append(readKeys, path)
		}
	}
	slices.Sort(readKeys)
	for _, path := range readKeys {
		read := item.KnownReads[path]
		parts = append(parts, fmt.Sprintf(
			"r:%s:%s:%d:%d:%s",
			path,
			read.Window,
			read.StartLine,
			read.EndLine,
			read.ContentDigest,
		))
	}
	editKeys := make([]string, 0, len(item.KnownEdits))
	for path := range item.KnownEdits {
		if strings.TrimSpace(path) != "" {
			editKeys = append(editKeys, path)
		}
	}
	slices.Sort(editKeys)
	for _, path := range editKeys {
		parts = append(parts, fmt.Sprintf(
			"e:%s:%s",
			path,
			item.KnownEdits[path].ContentDigest,
		))
	}
	return strings.Join(parts, ",")
}

func FormatResultDigest(digests []string) string {
	parts := make([]string, 0, len(digests))
	for _, digest := range digests {
		digest = strings.TrimSpace(digest)
		if digest != "" {
			parts = append(parts, digest)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	slices.Sort(parts)
	return strings.Join(slices.Compact(parts), "\x1e")
}

func FormatObservationKey(
	signature string,
	workspaceContent string,
	resultDigest string,
) string {
	sum := sha256.Sum256([]byte(
		strings.TrimSpace(signature) + "\x1e" +
			strings.TrimSpace(workspaceContent) + "\x1e" +
			strings.TrimSpace(resultDigest),
	))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ResultObservationDigest projects semantic execution facts, not the result's
// presentation or storage identity. Raw output and admission digests include
// timestamps, durations and temporary paths; process IDs/cursors and handles
// identify attempts, not progress. Unknown output conservatively adds no facts.
func ResultObservationDigest(result tool.Result) string {
	parts := []string{fmt.Sprintf("error=%t", result.IsError)}
	if result.Outcome != nil && result.Outcome.Facts != nil {
		facts := result.Outcome.Facts
		if read := facts.WorkspaceRead; read != nil {
			parts = append(parts, fmt.Sprintf("read:%q:%q", read.Path, read.Digest))
		}
		for _, change := range facts.WorkspaceChanges {
			parts = append(parts, fmt.Sprintf("change:%q:%q:%q", change.Path, change.Kind, change.AfterDigest))
		}
		for _, hit := range facts.Evidence {
			parts = append(parts, fmt.Sprintf("hit:%q:%q:%d:%q", hit.Kind, hit.Path, hit.Line, hit.Symbol))
		}
		for _, receipt := range facts.Diagnostics {
			parts = append(parts, fmt.Sprintf("diagnostics:%q:%q:%q:%q:%d", receipt.Path, receipt.Runner, receipt.Status, receipt.ErrorCategory, receipt.ExitCode))
			for _, diagnostic := range receipt.Diagnostics {
				parts = append(parts, fmt.Sprintf("diagnostic:%q:%v:%q:%q:%q", diagnostic.Path, diagnostic.Range, diagnostic.Severity, diagnostic.Code, diagnostic.Source))
			}
		}
		if verification := facts.Verification; verification != nil {
			parts = append(parts, fmt.Sprintf("verification:%q:%q:%q:%d", verification.Kind, verification.Status, verification.InputDigest, verification.ExitCode))
			for _, path := range verification.CoveredPaths {
				parts = append(parts, fmt.Sprintf("covered:%q", path))
			}
		}
		if failure := facts.Failure; failure != nil {
			parts = append(parts, fmt.Sprintf("failure:%q", failure.Category))
		}
		if session := facts.ProcessSession; session != nil {
			parts = append(parts, fmt.Sprintf("process:%t:%d:%t:%t", session.Running, session.ExitCode, session.Terminated, session.TimedOut))
		}
	}
	// Facts are sets. Producer ordering and repeated hits cannot renew a lease.
	slices.Sort(parts)
	sum := sha256.Sum256([]byte(strings.Join(slices.Compact(parts), "\n")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func rememberObservation(progress *ProgressState, key string) {
	if progress == nil || key == "" || progress.SeenObservations.contains(key) {
		return
	}
	if progress.SeenObservations == nil {
		progress.SeenObservations = &ObservationSet{}
	}
	progress.SeenObservations.add(key)
}

func seenObservation(progress ProgressState, key string) bool {
	if key == "" {
		return false
	}
	return progress.SeenObservations.contains(key)
}

func cloneProgressState(progress ProgressState) ProgressState {
	if progress.SeenObservations != nil {
		observations := progress.SeenObservations.clone()
		progress.SeenObservations = &observations
	}
	progress.PendingResultDigests = append(
		[]string(nil),
		progress.PendingResultDigests...,
	)
	return progress
}

func liveToolIdentity(identity string) bool {
	identity = strings.TrimSpace(identity)
	return identity != "" && identity != EmptySampleIdentity
}
