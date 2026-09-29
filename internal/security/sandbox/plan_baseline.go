package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

func VerifyPlanBaseline(root string, document json.RawMessage) error {
	if len(document) == 0 {
		return nil
	}
	var projection struct {
		FileBaseline []struct {
			Path    string `json:"path"`
			Digest  string `json:"digest"`
			Missing bool   `json:"missing"`
		} `json:"file_baseline"`
	}
	if err := json.Unmarshal(document, &projection); err != nil {
		return fmt.Errorf("decode Plan baseline: %w", err)
	}
	if len(projection.FileBaseline) == 0 {
		return nil
	}
	workspace, err := NewWorkspace(root)
	if err != nil {
		return err
	}
	for _, expected := range projection.FileBaseline {
		file, openErr := workspace.OpenFile(expected.Path)
		if errors.Is(openErr, os.ErrNotExist) {
			if expected.Missing {
				continue
			}
			return planDrift(expected.Path)
		}
		if openErr != nil {
			return fmt.Errorf("verify Plan baseline %q: %w", expected.Path, openErr)
		}
		if expected.Missing {
			_ = file.Close()
			return planDrift(expected.Path)
		}
		digest := sha256.New()
		_, copyErr := io.Copy(digest, file)
		closeErr := file.Close()
		if copyErr != nil || closeErr != nil {
			return fmt.Errorf(
				"verify Plan baseline %q: %w",
				expected.Path,
				errors.Join(copyErr, closeErr),
			)
		}
		if hex.EncodeToString(digest.Sum(nil)) != expected.Digest {
			return planDrift(expected.Path)
		}
	}
	return nil
}

// PlanDriftError reports a changed plan baseline without coupling verification to
// a host protocol. Callers decide how to present the conflict.
type PlanDriftError struct{ Path string }

func (e *PlanDriftError) Error() string {
	return fmt.Sprintf("Plan workspace baseline changed at %s; generate a new Plan revision", e.Path)
}

func planDrift(path string) error { return &PlanDriftError{Path: path} }
