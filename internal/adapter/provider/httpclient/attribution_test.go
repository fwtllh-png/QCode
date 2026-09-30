package httpclient

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestProviderRequestDeadlineReportsResponseHeaderScope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(
		http.ResponseWriter,
		*http.Request,
	) {
		time.Sleep(100 * time.Millisecond)
	}))
	defer server.Close()

	client := testClient()
	client.SetDeadlineConfig(DeadlineConfig{
		Connection:      time.Second,
		TLSHandshake:    time.Second,
		ResponseHeaders: 10 * time.Millisecond,
	})
	request := testRequest(t, server.URL, model.ProtocolOpenAIChat)
	request.LogicalRequestID = "sample-header-timeout"
	_, err := client.Stream(t.Context(), request)
	if !protocol.IsCode(err, protocol.CodeDeadlineExceeded) {
		t.Fatalf("Stream() error = %v", err)
	}
	problem := protocol.ProblemOf(err)
	if problem.Fault == nil ||
		problem.Fault.Stage != protocol.FaultStageResponseHeaders ||
		problem.Fault.OperationID != "sample-header-timeout" ||
		problem.Fault.RetryOwner != protocol.FaultRetryOwnerEngine ||
		problem.Fault.Deadline == nil ||
		problem.Fault.Deadline.Scope !=
			protocol.DeadlineProviderResponseHeaders ||
		problem.Fault.Deadline.TimeoutMS != 10 {
		t.Fatalf("response header fault = %+v", problem.Fault)
	}
}
