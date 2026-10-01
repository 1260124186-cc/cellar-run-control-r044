package workflowcheck

import (
	"fmt"
	"net/http/httptest"
	"os"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/clock"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/httpapi"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/service"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/storage"
)

func Run(name string) error {
	dataDir, err := os.MkdirTemp("", "cellar-run-check-*")
	if err != nil {
		return fmt.Errorf("create check directory: %w", err)
	}
	defer os.RemoveAll(dataDir)

	server, err := startCheckServer(dataDir)
	if err != nil {
		return err
	}
	defer server.Close()
	api := newClient(server.URL)

	switch name {
	case "formula-approval":
		if err := formulaApproval(api); err != nil {
			return err
		}
	case "vessel-run-lifecycle":
		if err := vesselRunLifecycle(api); err != nil {
			return err
		}
	case "observations-completion":
		if err := observationsCompletion(api); err != nil {
			return err
		}
	case "production-trajectory":
		if err := productionTrajectory(api, dataDir, server); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown workflow check %q", name)
	}
	fmt.Printf("workflow check passed: %s\n", name)
	return nil
}

// startCheckServer opens the snapshot directory and serves it on a fresh
// loopback listener. Calling it again with the same directory simulates a
// process restart against the persisted files.
func startCheckServer(dataDir string) (*httptest.Server, error) {
	store, err := storage.Open(dataDir)
	if err != nil {
		return nil, err
	}
	app := service.New(store, clock.System{})
	return httptest.NewServer(httpapi.NewHandler(app)), nil
}
