package environment

import "testing"

func TestClassifyFromAuthorityUsesTypedFactsOnly(t *testing.T) {
	category, action := ClassifyFromAuthority(nil)
	if category != CategoryUnknown || action != "" {
		t.Fatalf("empty facts = %q %q", category, action)
	}

	category, action = ClassifyFromAuthority([]Fact{{
		Source: SourceProcess,
		Detail: "401 Unauthorized\npermission denied\nGOPROXY",
	}})
	if category != CategoryUnknown || action != "" {
		t.Fatalf("untyped 401/permission text = %q %q", category, action)
	}

	category, action = ClassifyFromAuthority([]Fact{{
		Source:         SourcePreparer,
		Category:       CategoryEnvironmentResourceUnavailable,
		RequiredAction: ActionApproveHostConfig,
		Resource:       "host_config:goproxy",
		Detail:         "401 Unauthorized",
	}})
	if category != CategoryEnvironmentResourceUnavailable ||
		action != ActionApproveHostConfig {
		t.Fatalf("preparer fact = %q %q", category, action)
	}

	category, action = ClassifyFromAuthority([]Fact{
		{Source: SourceProcess, Detail: "permission denied"},
		{
			Source:         SourceOSBackend,
			Category:       CategoryFilesystemAccessDenied,
			RequiredAction: ActionEnableSharedUserTemp,
			Resource:       "shared_user_temp",
			Detail:         "permission denied",
		},
	})
	if category != CategoryFilesystemAccessDenied ||
		action != ActionEnableSharedUserTemp {
		t.Fatalf("os backend fact = %q %q", category, action)
	}

	category, action = ClassifyFromAuthority([]Fact{{
		Source:         SourceProcessProxy,
		Category:       CategoryNetworkTargetUnapproved,
		RequiredAction: ActionApproveNetworkTarget,
		Resource:       "https://code.byted.org:443",
		Detail:         "Forbidden",
	}})
	if category != CategoryNetworkTargetUnapproved ||
		action != ActionApproveNetworkTarget {
		t.Fatalf("gate fact = %q %q", category, action)
	}

	category, action = ClassifyFromAuthority([]Fact{{
		Source:         SourceGate,
		Category:       CategoryCredentialRejected,
		RequiredAction: ActionBindCredential,
		Detail:         "network unreachable",
	}})
	if category != CategoryCredentialRejected || action != ActionBindCredential {
		t.Fatalf("credential fact = %q %q", category, action)
	}
}
