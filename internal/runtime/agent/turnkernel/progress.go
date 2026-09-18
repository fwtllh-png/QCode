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
	return strings.Join(parts, "\x1e")
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

func ResultObservationDigest(result tool.Result) string {
	var builder strings.Builder
	if result.IsError {
		builder.WriteString("error")
	} else {
		builder.WriteString("ok")
	}
	switch {
	case result.Admission != nil &&
		strings.TrimSpace(result.Admission.Digest) != "":
		builder.WriteString(";admission=")
		builder.WriteString(strings.TrimSpace(result.Admission.Digest))
	case result.Content != "":
		sum := sha256.Sum256([]byte(result.Content))
		builder.WriteString(";content=sha256:")
		builder.WriteString(hex.EncodeToString(sum[:]))
	}
	if result.Outcome == nil || result.Outcome.Facts == nil {
		return builder.String()
	}
	facts := result.Outcome.Facts
	if read := facts.WorkspaceRead; read != nil {
		if digest := strings.TrimSpace(read.Digest); digest != "" {
			builder.WriteString(";read=")
			builder.WriteString(digest)
		}
	}
	if session := facts.ProcessSession; session != nil {
		fmt.Fprintf(
			&builder,
			";proc=%s:%d:%d:%t",
			strings.TrimSpace(session.SessionID),
			session.Cursor,
			session.ExitCode,
			session.Running,
		)
	}
	return builder.String()
}

func rememberObservation(progress *ProgressState, key string) {
	if progress == nil || key == "" || seenObservation(*progress, key) {
		return
	}
	progress.SeenObservations = append(
		append([]string(nil), progress.SeenObservations...),
		key,
	)
}

func seenObservation(progress ProgressState, key string) bool {
	if key == "" {
		return false
	}
	return slices.Contains(progress.SeenObservations, key)
}

func cloneProgressState(progress ProgressState) ProgressState {
	progress.SeenObservations = append([]string(nil), progress.SeenObservations...)
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
