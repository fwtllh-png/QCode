package engine

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/turnhistory"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
)

func TestConversationDurableSessionAcrossTurnsAndRestore(t *testing.T) {
	p := &scriptedProvider{streams: []provider.Stream{textStream("1. 第一项定义\n2. 需要保留的第二项定义"), textStream("第二轮进度")}}
	e := newEngine(t, p, tool.NewRegistry(nil, nil))
	e.options.Workspace = t.TempDir()
	e.options.SessionID = "process-before-restart"
	e.options.Context.SemanticNarrative = "off"
	e.options.Context.RecentTailTurns = 1
	owners := map[string]string{"report": "session-real", "followup": "session-real", "restored": "session-real"}
	e.options.SessionForTurn = func(_ context.Context, turn string) (string, bool) {
		owner, ok := owners[turn]
		return owner, ok
	}
	var identities []TurnIdentity
	e.options.ReleaseTurnResources = func(identity TurnIdentity) { identities = append(identities, identity) }
	ctx := tool.WithInvocationIdentity(t.Context(), tool.InvocationIdentity{SessionID: "session-real", ThreadID: "thread"})
	for _, turn := range []string{"report", "followup"} {
		if _, err := e.Execute(ctx, TurnRequest{TurnID: turn, Prompt: "继续分析第二项"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if len(identities) != 2 || identities[0].SessionID != "session-real" || identities[1].SessionID != "session-real" {
		t.Fatalf("turns did not freeze durable session identity: %+v", identities)
	}
	if len(p.requests) != 2 || !strings.Contains(joinMessageText(p.requests[1].Messages), "需要保留的第二项定义") {
		t.Fatal("followup lost the original source")
	}
	snapshot, err := e.ExportContextSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	source := snapshot.Conversation.SourcesForTurn(1)[0]
	restartedProvider := &scriptedProvider{streams: []provider.Stream{textStream("恢复后继续")}}
	restarted := newEngine(t, restartedProvider, tool.NewRegistry(nil, nil))
	restarted.options.Workspace = e.options.Workspace
	restarted.options.SessionID = "process-after-restart"
	restarted.options.SessionForTurn = e.options.SessionForTurn
	restarted.options.Context = e.options.Context
	if _, err := restarted.RestoreContextSnapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	entry, err := restarted.lookupConversationEntry(ctx, turnhistory.ReferenceRequest{SourceID: source.ID})
	if err != nil || entry == nil || !strings.Contains(entry.Transcript, "需要保留的第二项定义") {
		t.Fatalf("restored source recovery: entry=%+v err=%v", entry, err)
	}
	if _, err := restarted.Execute(ctx, TurnRequest{TurnID: "restored", Prompt: "继续第二项"}, nil); err != nil {
		t.Fatal(err)
	}
	if len(restartedProvider.requests) != 1 || !strings.Contains(joinMessageText(restartedProvider.requests[0].Messages), "需要保留的第二项定义") {
		t.Fatal("restored source did not reach the provider")
	}
	if got := restarted.context.Conversation().Sources[source.ID]; !reflect.DeepEqual(got, source) {
		t.Fatal("restart changed source identity or bytes")
	}
}

func TestConversationSessionBoundaryBeforeSampling(t *testing.T) {
	for _, tc := range []struct {
		name, session, owner string
		found, withdrawn     bool
		allowed              bool
	}{
		{name: "same-session", session: "session-real", owner: "session-real", found: true, allowed: true},
		{name: "foreign-session", session: "session-other", owner: "session-real", found: true},
		{name: "missing-owner", session: "session-real"},
		{name: "empty-owner", session: "session-real", found: true},
		{name: "withdrawn", session: "session-real", owner: "session-real", found: true, withdrawn: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &scriptedProvider{streams: []provider.Stream{textStream("继续")}}
			e := newEngine(t, p, tool.NewRegistry(nil, nil))
			e.options.Workspace = t.TempDir()
			e.options.SessionID = "process-session"
			e.options.SessionForTurn = func(context.Context, string) (string, bool) { return tc.owner, tc.found }
			source := agentcontext.IndexConversationAnswer("thread", "report", 1, "1. 受会话边界保护的原始定义")
			state := &agentcontext.ConversationState{}
			if err := state.Add(source); err != nil {
				t.Fatal(err)
			}
			// The current request explicitly depends on this source. Optional
			// unavailable candidates are tested separately and may be omitted.
			if err := state.Select(agentcontext.NewConversationSelection(nil, []string{source.Items[0].ID}, 2, "followup", "继续第一项"), nil); err != nil {
				t.Fatal(err)
			}
			e.context.SetConversation(state)
			e.turn = 1
			e.options.TurnContexts = &withdrawalContextStore{withdrawn: tc.withdrawn}
			ctx := tool.WithInvocationIdentity(t.Context(), tool.InvocationIdentity{SessionID: tc.session, ThreadID: "thread"})
			entry, err := e.lookupConversationEntry(ctx, turnhistory.ReferenceRequest{SourceID: source.ID})
			if err != nil || (entry != nil) != tc.allowed {
				t.Fatalf("source lookup: entry=%+v err=%v", entry, err)
			}
			_, err = e.Execute(ctx, TurnRequest{TurnID: "followup", Prompt: "继续第一项"}, nil)
			if (err == nil) != tc.allowed || (len(p.requests) != 0) != tc.allowed {
				t.Fatalf("sampling boundary: err=%v requests=%d", err, len(p.requests))
			}
		})
	}
}

func TestConversationRecoveryUsesFrozenScopeSession(t *testing.T) {
	e := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	e.options.SessionID = "process-session"
	e.options.SessionForTurn = func(context.Context, string) (string, bool) { return "session-real", true }
	source := agentcontext.IndexConversationAnswer("thread", "report", 1, "1. 原始定义")
	state := &agentcontext.ConversationState{}
	if err := state.Add(source); err != nil {
		t.Fatal(err)
	}
	e.context.SetConversation(state)
	// A callback may arrive without invocation identity, or with a different
	// one. Only the admitted Scope can authorize its source reads.
	for _, session := range []string{"session-real", "session-other"} {
		t.Run(session, func(t *testing.T) {
			scope := attachTestScope(t, e)
			scope.spec.Identity.SessionID = session
			for _, callbackSession := range []string{"", "session-real", "session-other"} {
				ctx := tool.WithSessionIdentity(t.Context(), callbackSession)
				entry, err := e.lookupConversationEntry(ctx, turnhistory.ReferenceRequest{SourceID: source.ID})
				if err != nil || (entry != nil) != (session == "session-real") {
					t.Fatalf("scope=%s callback=%s: entry=%+v err=%v", session, callbackSession, entry, err)
				}
			}
		})
	}
}
