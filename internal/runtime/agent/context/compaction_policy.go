package agentcontext

import "github.com/fwtllh-png/QCode/internal/adapter/provider"

type WindowMeasurement struct {
	Estimated    uint64
	Total        uint64
	Active       uint64
	HardLimit    uint64
	CompactLimit uint64
	Projection   WindowProjection
}

type SurfacePruning struct {
	Results int
	Bytes   int
}

type HistoryProjector func([]provider.Message) []provider.Message

func ProjectHistory(
	history []provider.Message,
	project HistoryProjector,
) []provider.Message {
	if project != nil {
		return project(history)
	}
	return history
}

type CompactionSelectionRequest struct {
	History             []provider.Message
	Force               bool
	AllowCurrentTurn    bool
	Input               MessageSnapshot
	OutputReserve       uint64
	RecentTailTurns     int
	RecentTailMaxTokens uint64
	WindowScope         string
	AuthorityDigest     string
	EstimateMessages    func([]provider.Message) uint64
	// ProjectHistory builds the exact Provider-visible history for measurement.
	// Durable History remains the authority used to build compaction candidates.
	ProjectHistory HistoryProjector
	Measure        func(MessageSnapshot, uint64) (WindowMeasurement, error)
	// PruneBeforePressure shrinks already-consumed, handle-backed tool results
	// using the caller's dynamic surface budget even when compaction is not due.
	PruneBeforePressure bool
	Prune               func(
		*[]provider.Message,
		MessageSnapshot,
		uint64,
		bool,
	) (SurfacePruning, WindowMeasurement, error)
	Build func([]provider.Message, int, bool) (CompactionCandidate, error)
}

type CompactionSelection struct {
	History          []provider.Message
	OriginalMessages int
	OriginalBytes    int
	OriginalWindow   WindowMeasurement
	RetainedWindow   WindowMeasurement
	Pruning          SurfacePruning
	Candidate        *CompactionCandidate
}

func SelectCompaction(
	request CompactionSelectionRequest,
) (CompactionSelection, error) {
	result := CompactionSelection{
		History:          CloneMessages(request.History),
		OriginalMessages: len(request.History),
		OriginalBytes:    HistoryBytes(request.History),
	}
	project := func(history []provider.Message) MessageSnapshot {
		return request.Input.WithHistory(
			ProjectHistory(history, request.ProjectHistory),
		)
	}
	input := project(request.History)
	original, err := request.Measure(input, request.OutputReserve)
	if err != nil {
		return CompactionSelection{}, err
	}
	result.OriginalWindow = original
	belowPressure := !request.Force &&
		original.Active < original.CompactLimit &&
		original.Total <= original.HardLimit
	if belowPressure && !request.PruneBeforePressure {
		return result, nil
	}
	working := CloneMessages(request.History)
	pruned, prunedWindow, err := request.Prune(
		&working,
		request.Input,
		request.OutputReserve,
		request.Force,
	)
	if err != nil {
		return CompactionSelection{}, err
	}
	result.Pruning = pruned
	result.RetainedWindow = prunedWindow
	if pruned.Results != 0 &&
		!ToolPairIdentityEquivalent(request.History, working) {
		return result, nil
	}
	if belowPressure {
		if pruned.Results != 0 {
			result.History = working
		}
		return result, nil
	}
	pruningEnough := pruned.Results != 0 &&
		(request.RecentTailMaxTokens == 0 ||
			request.EstimateMessages(working) <= request.RecentTailMaxTokens) &&
		(prunedWindow.Active <= prunedWindow.CompactLimit &&
			prunedWindow.Total <= prunedWindow.HardLimit ||
			request.Force && original.Total <= original.HardLimit)
	if pruningEnough {
		result.History = working
		return result, nil
	}
	workingWindow := original
	if pruned.Results != 0 {
		workingWindow = prunedWindow
	}
	cuts := RetainedTailCuts(
		working,
		request.AllowCurrentTurn,
		request.RecentTailTurns,
		request.RecentTailMaxTokens,
		request.EstimateMessages,
	)
	if accept := selectReducingCandidate(
		request, working, cuts, original, workingWindow, input, project, &result,
	); accept {
		return result, nil
	}
	if request.Force {
		if accept := selectReducingCandidate(
			request,
			working,
			HistoryCuts(working, request.AllowCurrentTurn),
			original,
			workingWindow,
			input,
			project,
			&result,
		); accept {
			return result, nil
		}
	}
	if pruned.Results != 0 {
		result.History = working
	}
	return result, nil
}

func selectReducingCandidate(
	request CompactionSelectionRequest,
	working []provider.Message,
	cuts []int,
	original WindowMeasurement,
	workingWindow WindowMeasurement,
	input MessageSnapshot,
	project func([]provider.Message) MessageSnapshot,
	result *CompactionSelection,
) bool {
	for _, cut := range cuts {
		candidate, buildErr := request.Build(working, cut, true)
		if buildErr != nil {
			continue
		}
		window, measureErr := request.Measure(
			project(candidate.History),
			request.OutputReserve,
		)
		if measureErr == nil && window.Active > original.CompactLimit {
			minimal, minimalErr := request.Build(working, cut, false)
			if minimalErr == nil {
				minimalWindow, minimalMeasureErr := request.Measure(
					project(minimal.History),
					request.OutputReserve,
				)
				if minimalMeasureErr == nil &&
					minimalWindow.Active < window.Active {
					candidate, window = minimal, minimalWindow
				}
			}
		}
		if measureErr != nil ||
			window.Active >= workingWindow.Active &&
				window.Total >= workingWindow.Total ||
			!ToolPairsClosed(candidate.History) ||
			candidate.Capsule.ContainsAuthority(candidate.Authority) != nil {
			continue
		}
		authorityDigest, digestErr := candidate.Authority.AuthorityDigest()
		if digestErr != nil {
			continue
		}
		candidate.AuthorityDigest = authorityDigest
		candidate.SourceWindowID = original.Projection.ID
		candidate.SourceContextDigest, _ = input.Digest()
		candidate.SourceHistory = CloneMessages(request.History)
		candidate.SourceHistoryDigest = HistoryDigest(request.History)
		candidate.SourceTokens = original.Active
		candidate.RetainedTokens = window.Active
		if request.Force ||
			window.Active <= original.CompactLimit ||
			request.WindowScope == "body_after_prefix" &&
				original.Total > original.HardLimit &&
				window.Total <= window.HardLimit {
			result.History = candidate.History
			result.RetainedWindow = window
			result.Candidate = &candidate
			return true
		}
	}
	return false
}
