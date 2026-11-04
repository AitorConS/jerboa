package attest

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// GitHubBuildType is the SLSA build type of a GitHub Actions workflow run,
// the one GitHub's own artifact attestations use.
const GitHubBuildType = "https://actions.github.io/buildtypes/workflow/v1"

// GitHub describes the GitHub Actions job an image is published from, as the
// job's environment gives it.
type GitHub struct {
	ServerURL         string
	Repository        string
	RepositoryID      string
	RepositoryOwnerID string
	SHA               string
	Ref               string
	// WorkflowRef is owner/repo/path@ref of the workflow that runs the job.
	WorkflowRef       string
	RunID             string
	RunAttempt        string
	EventName         string
	RunnerEnvironment string
}

// GitHubFromEnv reads the job's description from the variables GitHub
// Actions sets.
func GitHubFromEnv() (GitHub, error) {
	g := GitHub{
		ServerURL:         os.Getenv("GITHUB_SERVER_URL"),
		Repository:        os.Getenv("GITHUB_REPOSITORY"),
		RepositoryID:      os.Getenv("GITHUB_REPOSITORY_ID"),
		RepositoryOwnerID: os.Getenv("GITHUB_REPOSITORY_OWNER_ID"),
		SHA:               os.Getenv("GITHUB_SHA"),
		Ref:               os.Getenv("GITHUB_REF"),
		WorkflowRef:       os.Getenv("GITHUB_WORKFLOW_REF"),
		RunID:             os.Getenv("GITHUB_RUN_ID"),
		RunAttempt:        os.Getenv("GITHUB_RUN_ATTEMPT"),
		EventName:         os.Getenv("GITHUB_EVENT_NAME"),
		RunnerEnvironment: os.Getenv("RUNNER_ENVIRONMENT"),
	}
	if os.Getenv("GITHUB_ACTIONS") != "true" || g.ServerURL == "" || g.Repository == "" || g.SHA == "" || g.WorkflowRef == "" || g.RunID == "" {
		return GitHub{}, errors.New("attest: provenance is recorded from a GitHub Actions job, and this is not one")
	}
	return g, nil
}

// Provenance returns the SLSA v1 provenance predicate of the job.
func (g GitHub) Provenance() ([]byte, error) {
	path, ref, ok := strings.Cut(g.WorkflowRef, "@")
	repo := g.ServerURL + "/" + g.Repository
	workflowPath, found := strings.CutPrefix(path, g.Repository+"/")
	if !ok || !found {
		return nil, fmt.Errorf("attest: unexpected workflow ref %q", g.WorkflowRef)
	}
	type digest map[string]string
	p := map[string]any{
		"buildDefinition": map[string]any{
			"buildType": GitHubBuildType,
			"externalParameters": map[string]any{
				"workflow": map[string]string{"ref": ref, "repository": repo, "path": workflowPath},
			},
			"internalParameters": map[string]any{
				"github": map[string]string{
					"event_name":          g.EventName,
					"repository_id":       g.RepositoryID,
					"repository_owner_id": g.RepositoryOwnerID,
					"runner_environment":  g.RunnerEnvironment,
				},
			},
			"resolvedDependencies": []map[string]any{
				{"uri": "git+" + repo + "@" + g.Ref, "digest": digest{"gitCommit": g.SHA}},
			},
		},
		"runDetails": map[string]any{
			"builder": map[string]string{"id": g.ServerURL + "/" + g.WorkflowRef},
			"metadata": map[string]string{
				"invocationId": repo + "/actions/runs/" + g.RunID + "/attempts/" + g.RunAttempt,
			},
		},
	}
	data, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("attest: encode provenance: %w", err)
	}
	return data, nil
}
