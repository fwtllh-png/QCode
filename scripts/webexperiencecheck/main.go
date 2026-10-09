package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

type contract struct {
	Version          int               `json:"version"`
	Stylesheets      []string          `json:"stylesheets"`
	LayoutRegions    []string          `json:"layout_regions"`
	ResponsiveTracks []responsiveTrack `json:"responsive_tracks"`
	SemanticTokens   []string          `json:"semantic_tokens"`
	CanonicalStates  []string          `json:"canonical_states"`
	Motion           []motionRule      `json:"motion"`
	FocusOrder       []string          `json:"focus_order"`
	KeyboardCommands []keyboardCommand `json:"keyboard_commands"`
	StableGeometry   []string          `json:"stable_geometry"`
	Behavior         map[string]string `json:"behavior"`
	Viewports        []string          `json:"viewports"`
	Themes           []string          `json:"themes"`
	ZoomPercent      []int             `json:"zoom_percent"`
	GoldenDirectory  string            `json:"golden_directory"`
	Goldens          []string          `json:"goldens"`
	CSSPolicy        cssPolicy         `json:"css_policy"`
}

type responsiveTrack struct {
	Mode       string   `json:"mode"`
	MinWidth   int      `json:"min_width"`
	MaxWidth   int      `json:"max_width"`
	Tracks     string   `json:"tracks"`
	YieldOrder []string `json:"yield_order"`
}

type motionRule struct {
	ID       string `json:"id"`
	Selector string `json:"selector"`
	Full     string `json:"full"`
	Reduced  string `json:"reduced"`
	Still    string `json:"still"`
}

type keyboardCommand struct {
	ID             string `json:"id"`
	Key            string `json:"key"`
	AccessibleName string `json:"accessible_name"`
}

type cssPolicy struct {
	SemanticPrefix                  string `json:"semantic_prefix"`
	MaxCardRadiusPX                 int    `json:"max_card_radius_px"`
	ForbidTransitionAll             bool   `json:"forbid_transition_all"`
	RequireReducedMotionForInfinite bool   `json:"require_reduced_motion_for_infinite"`
	RequireRegisteredZIndex         bool   `json:"require_registered_z_index"`
}

var (
	colorLiteral  = regexp.MustCompile(`(?i)(#[0-9a-f]{3,8}\b|(?:rgb|hsl)a?\()`)
	radiusLiteral = regexp.MustCompile(`border-radius:\s*(\d+)px`)
	zIndexLiteral = regexp.MustCompile(`z-index:\s*([^;}]+)`)
)

func main() {
	root := "."
	path := "testdata/contracts/web-experience-contract.json"
	if len(os.Args) > 3 {
		fmt.Fprintln(os.Stderr, "usage: webexperiencecheck [contract] [root]")
		os.Exit(2)
	}
	if len(os.Args) >= 2 {
		path = os.Args[1]
	}
	if len(os.Args) == 3 {
		root = os.Args[2]
	}
	value, err := readContract(filepath.Join(root, path))
	if err == nil {
		err = validate(value, root)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Web experience contract invalid:", err)
		os.Exit(1)
	}
	fmt.Printf(
		"Web experience contract valid: %d viewports, %d states, %d tokens\n",
		len(value.Viewports),
		len(value.CanonicalStates),
		len(value.SemanticTokens),
	)
}

func readContract(path string) (contract, error) {
	file, err := os.Open(path)
	if err != nil {
		return contract{}, err
	}
	defer file.Close()
	var value contract
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return contract{}, err
	}
	return value, nil
}

func validate(value contract, root string) error {
	if value.Version != 1 {
		return fmt.Errorf("version = %d, want 1", value.Version)
	}
	if !sameSet(value.Stylesheets, []string{
		"web/src/ui/styles.css",
		"web/src/ui/SettingsDialog.css",
		"web/src/ui/WorkspaceContextDialog.css",
		"web/src/ui/Trajectory.css",
		"web/src/ui/theme/tokens.css",
		"web/src/ui/primitives/primitives.css",
		"web/src/ui/GitTools.css",
	}) {
		return errors.New("stylesheets must cover the production Web styles")
	}
	if !sameSet(value.LayoutRegions, []string{
		"session_rail", "conversation", "composer", "settings_dialog",
		"context_dialog", "trajectory_inspector",
		"git_tools",
	}) {
		return errors.New("layout regions drifted")
	}
	if err := validateTracks(value.ResponsiveTracks); err != nil {
		return err
	}
	if !sameSet(value.CanonicalStates, []string{
		"empty", "blank_session", "streaming", "tool_collapsed",
		"tool_expanded", "approval", "input", "failure", "completed",
		"diff", "settings", "settings_models", "settings_tools",
		"settings_agent", "context_browser", "edit_approval", "trajectory",
	}) {
		return errors.New("canonical states drifted")
	}
	if !sameSet(value.Viewports, []string{
		"390x844", "1024x768", "1440x900", "1920x1080",
	}) {
		return errors.New("viewport matrix drifted")
	}
	if !sameSet(value.Themes, []string{"light", "dark", "forced-colors"}) {
		return errors.New("theme matrix drifted")
	}
	if !slices.Equal(value.ZoomPercent, []int{100, 200}) {
		return errors.New("zoom matrix must be 100 and 200 percent")
	}
	if err := validateGoldens(value, root); err != nil {
		return err
	}
	if len(value.FocusOrder) < 8 || len(value.KeyboardCommands) < 3 ||
		len(value.StableGeometry) < 8 || len(value.Motion) == 0 {
		return errors.New("interaction, geometry, or motion matrix is incomplete")
	}
	if hasDuplicate(value.SemanticTokens) || hasDuplicate(value.StableGeometry) {
		return errors.New("tokens and stable geometry selectors must be unique")
	}
	if len(value.Behavior) != 6 {
		return errors.New("behavior matrix is incomplete")
	}
	if value.CSSPolicy.SemanticPrefix != "--ch-" ||
		value.CSSPolicy.MaxCardRadiusPX != 24 ||
		!value.CSSPolicy.ForbidTransitionAll ||
		!value.CSSPolicy.RequireReducedMotionForInfinite ||
		!value.CSSPolicy.RequireRegisteredZIndex {
		return errors.New("CSS policy must preserve all static safety gates")
	}
	var content strings.Builder
	for _, stylesheet := range value.Stylesheets {
		value, err := os.ReadFile(filepath.Join(root, stylesheet))
		if err != nil {
			return err
		}
		content.Write(value)
		content.WriteByte('\n')
	}
	return validateCSS(value, content.String())
}

func validateGoldens(value contract, root string) error {
	const directory = "web/tests/e2e/visual.spec.ts-snapshots"
	expected := []string{
		"canonical-approval.png",
		"canonical-back-to-bottom.png",
		"canonical-blank-session.png",
		"canonical-completed.png",
		"canonical-diff.png",
		"canonical-edit-approval.png",
		"canonical-edit-completed.png",
		"canonical-empty.png",
		"canonical-failure.png",
		"canonical-input.png",
		"canonical-settings.png",
		"canonical-settings-models.png",
		"canonical-settings-tools.png",
		"canonical-settings-agent.png",
		"canonical-context-browser.png",
		"canonical-streaming.png",
		"canonical-tool-collapsed.png",
		"canonical-tool-expanded.png",
		"canonical-trajectory.png",
		"canonical-trajectory-detail.png",
		"viewport-390x844-dark.png",
		"viewport-390x844-light.png",
		"viewport-1024x768-dark.png",
		"viewport-1024x768-forced-colors.png",
		"viewport-1024x768-light.png",
		"viewport-1024x768-zoom-200.png",
		"viewport-1440x900-dark.png",
		"viewport-1440x900-light.png",
		"viewport-1920x1080-dark.png",
		"viewport-1920x1080-light.png",
	}
	if value.GoldenDirectory != directory || !sameSet(value.Goldens, expected) {
		return errors.New("visual golden matrix drifted")
	}
	pngSignature := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	for _, name := range value.Goldens {
		if filepath.Base(name) != name {
			return fmt.Errorf("visual golden %q is not a file name", name)
		}
		content, err := os.ReadFile(filepath.Join(root, directory, name))
		if err != nil {
			return fmt.Errorf("read visual golden %q: %w", name, err)
		}
		if len(content) < 1024 || !bytes.HasPrefix(content, pngSignature) {
			return fmt.Errorf("visual golden %q is not a complete PNG", name)
		}
	}
	return nil
}

func validateTracks(tracks []responsiveTrack) error {
	if len(tracks) != 3 {
		return errors.New("responsive tracks must define compact, regular, and wide")
	}
	slices.SortFunc(tracks, func(left, right responsiveTrack) int {
		return left.MinWidth - right.MinWidth
	})
	for index, want := range []string{"compact", "regular", "wide"} {
		track := tracks[index]
		if track.Mode != want || track.MinWidth < 320 ||
			track.MaxWidth < track.MinWidth || track.Tracks == "" {
			return fmt.Errorf("responsive track %q is incomplete", track.Mode)
		}
		if index > 0 && tracks[index-1].MaxWidth+1 != track.MinWidth {
			return errors.New("responsive track ranges contain a gap or overlap")
		}
	}
	return nil
}

func validateCSS(value contract, content string) error {
	for _, token := range value.SemanticTokens {
		if !strings.Contains(content, token+":") {
			return fmt.Errorf("semantic token %q is not declared", token)
		}
	}
	for _, selector := range value.StableGeometry {
		if !strings.Contains(content, selector) {
			return fmt.Errorf("stable geometry selector %q is not styled", selector)
		}
	}
	for _, motion := range value.Motion {
		if motion.ID == "" || motion.Selector == "" || motion.Full == "" ||
			motion.Reduced == "" || motion.Still == "" ||
			!strings.Contains(content, motion.Selector) {
			return fmt.Errorf("motion rule %q is incomplete", motion.ID)
		}
	}
	lower := strings.ToLower(content)
	if strings.Contains(lower, "transition: all") {
		return errors.New("transition: all is forbidden")
	}
	if strings.Contains(lower, "infinite") &&
		!strings.Contains(lower, "@media (prefers-reduced-motion: reduce)") {
		return errors.New("infinite animation has no reduced-motion fallback")
	}
	scanner := bufio.NewScanner(strings.NewReader(content))
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if colorLiteral.MatchString(line) && !strings.HasPrefix(line, "--ch-") {
			return fmt.Errorf("line %d uses an unregistered color literal", lineNumber)
		}
		if match := zIndexLiteral.FindStringSubmatch(line); len(match) == 2 &&
			!strings.Contains(match[1], "var(--ch-layer-") {
			return fmt.Errorf("line %d uses an unregistered z-index", lineNumber)
		}
		if match := radiusLiteral.FindStringSubmatch(line); len(match) == 2 {
			radius, _ := strconv.Atoi(match[1])
			if radius > value.CSSPolicy.MaxCardRadiusPX {
				return fmt.Errorf("line %d uses %dpx radius", lineNumber, radius)
			}
		}
	}
	return scanner.Err()
}

func sameSet[T cmpOrdered](left, right []T) bool {
	left = append([]T(nil), left...)
	right = append([]T(nil), right...)
	slices.Sort(left)
	slices.Sort(right)
	return slices.Equal(left, right)
}

type cmpOrdered interface {
	~int | ~string
}

func hasDuplicate(values []string) bool {
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if seen[value] {
			return true
		}
		seen[value] = true
	}
	return false
}
