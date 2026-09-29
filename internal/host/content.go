package host

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

const contentHandleTTL = 5 * time.Minute

type contentHandle struct {
	Version         int    `json:"version"`
	WorkspaceRootID string `json:"workspace_root_id"`
	Path            string `json:"path"`
	Digest          string `json:"digest"`
	MediaType       string `json:"media_type,omitempty"`
	ExpiresAt       int64  `json:"expires_at"`
}

func (s *Server) issueContentHandle(
	identity protocol.WorkspaceIdentity,
	resourcePath, digest string,
	mediaTypes ...string,
) (string, error) {
	mediaType := ""
	if len(mediaTypes) > 0 {
		mediaType = mediaTypes[0]
	}
	payload, err := json.Marshal(contentHandle{
		Version: 1, WorkspaceRootID: identity.RootID,
		Path: resourcePath, Digest: digest, MediaType: mediaType,
		ExpiresAt: time.Now().UTC().Add(contentHandleTTL).Unix(),
	})
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(s.token))
	_, _ = mac.Write([]byte(encoded))
	return encoded + "." + hex.EncodeToString(mac.Sum(nil)), nil
}

func (s *Server) parseContentHandle(value string) (contentHandle, error) {
	if value == "" || len(value) > 4096 || strings.Contains(value, "/") {
		return contentHandle{}, errors.New("content handle is invalid")
	}
	encoded, signature, found := strings.Cut(value, ".")
	if !found {
		return contentHandle{}, errors.New("content handle is invalid")
	}
	provided, err := hex.DecodeString(signature)
	if err != nil {
		return contentHandle{}, errors.New("content handle is invalid")
	}
	mac := hmac.New(sha256.New, []byte(s.token))
	_, _ = mac.Write([]byte(encoded))
	if !hmac.Equal(provided, mac.Sum(nil)) {
		return contentHandle{}, errors.New("content handle is invalid")
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return contentHandle{}, errors.New("content handle is invalid")
	}
	var handle contentHandle
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&handle); err != nil ||
		handle.Version != 1 ||
		handle.WorkspaceRootID == "" ||
		handle.Path == "" ||
		len(handle.Digest) != sha256.Size*2 ||
		(handle.MediaType != "" && !supportedImageMediaType(handle.MediaType)) ||
		handle.ExpiresAt < time.Now().UTC().Unix() {
		return contentHandle{}, errors.New("content handle is invalid or expired")
	}
	return handle, nil
}

func (s *Server) content(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	if !s.authorized(r) {
		writeProblem(w, r, http.StatusUnauthorized, protocol.NewProblem(
			protocol.CodeUnavailable,
			"web capability token is missing or invalid",
			false,
			nil,
		))
		return
	}
	if !s.ready.Load() || s.draining.Load() {
		writeProblem(w, r, http.StatusServiceUnavailable, protocol.NewProblem(
			protocol.CodeUnavailable,
			"Runtime is not ready",
			true,
			nil,
		))
		return
	}
	handle, err := s.parseContentHandle(
		strings.TrimPrefix(r.URL.Path, "/api/v1/content/"),
	)
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, protocol.NewProblem(
			protocol.CodeInvalidArgument,
			"content handle is invalid or expired",
			false,
			nil,
		))
		return
	}
	dependencies, _, found := s.workspaceSnapshot(r.Header.Get(workspaceHeader))
	if !found {
		writeProblem(w, r, http.StatusNotFound, protocol.NewProblem(
			protocol.CodeInvalidArgument,
			"workspace is not registered with this Web Host",
			false,
			nil,
		))
		return
	}
	if dependencies.Workspace == nil ||
		handle.WorkspaceRootID != dependencies.WorkspaceIdentity.RootID {
		writeProblem(w, r, http.StatusForbidden, protocol.NewProblem(
			protocol.CodeConflict,
			"content handle does not belong to this Workspace",
			false,
			nil,
		))
		return
	}
	var (
		resourcePath string
		digest       string
		mediaType    = "text/plain; charset=utf-8"
		data         []byte
	)
	if handle.MediaType == "" {
		resource, resourceErr := dependencies.Workspace.Resource(r.Context(), handle.Path)
		if resourceErr != nil {
			writeApplicationError(w, r, workspaceQueryError(resourceErr))
			return
		}
		resourcePath, digest = resource.Path, resource.Digest
		data = []byte(resource.Content)
	} else {
		image, imageErr := dependencies.Workspace.Image(r.Context(), handle.Path)
		if imageErr != nil {
			writeApplicationError(w, r, workspaceQueryError(imageErr))
			return
		}
		if image.MediaType != handle.MediaType {
			writeProblem(w, r, http.StatusConflict, protocol.NewProblem(
				protocol.CodeConflict,
				"content handle media type is stale",
				true,
				nil,
			))
			return
		}
		resourcePath, digest, mediaType, data =
			image.Path, image.Digest, image.MediaType, image.Data
	}
	if digest != handle.Digest {
		writeProblem(w, r, http.StatusConflict, protocol.NewProblem(
			protocol.CodeConflict,
			"content handle is stale",
			true,
			nil,
		))
		return
	}
	etag := `"` + digest + `"`
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set(
		"Content-Disposition",
		mime.FormatMediaType("attachment", map[string]string{
			"filename": path.Base(resourcePath),
		}),
	)
	w.Header().Set("ETag", etag)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func supportedImageMediaType(value string) bool {
	switch value {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	default:
		return false
	}
}
