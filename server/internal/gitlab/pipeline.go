package gitlab

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"devops/internal/scm"
)

// The pipeline event (LAUN-85, decisions-T17 #19): "Pipeline Hook". It names a
// commit, not a merge request, and the tracker applies it to the links whose
// head sha is that commit. The shape is GitLab's documented pipeline payload;
// the fixtures are hand-written (testdata/README.md).

// defaultCheckName is the check name of a pipeline that has no name.
const defaultCheckName = "pipeline"

// PipelineEvent is the part of a pipeline payload the tracker reads.
type PipelineEvent struct {
	ObjectKind string `json:"object_kind"`
	Project    struct {
		ID                int64  `json:"id"`
		PathWithNamespace string `json:"path_with_namespace"`
	} `json:"project"`
	Attrs struct {
		SHA        string `json:"sha"`
		Status     string `json:"status"`
		Name       string `json:"name"`
		CreatedAt  string `json:"created_at"`
		FinishedAt string `json:"finished_at"`
	} `json:"object_attributes"`
}

// ParsePipeline decodes a pipeline payload and checks the fields the tracker
// cannot work without.
func ParsePipeline(body []byte) (PipelineEvent, error) {
	var ev PipelineEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return PipelineEvent{}, fmt.Errorf("invalid JSON body: %w", err)
	}
	switch {
	case ev.ObjectKind != "pipeline":
		return PipelineEvent{}, errors.New("object_kind must be pipeline")
	case ev.Project.ID < 1:
		return PipelineEvent{}, errors.New("project.id is missing")
	case ev.Attrs.SHA == "":
		return PipelineEvent{}, errors.New("object_attributes.sha is missing")
	}
	return ev, nil
}

// pipelineState maps a pipeline status: success and skipped are success;
// failed and canceled are failure; every state before the end (created,
// waiting_for_resource, preparing, pending, running, scheduled, manual, and
// canceling, which newer GitLab sends while a cancel is under way) is
// pending. A pipeline that waits on a manual job stays pending until it ends;
// skipped counts as success (nothing ran, nothing failed). ok is false for a status the tracker does not know.
func pipelineState(status string) (scm.ChecksState, bool) {
	switch status {
	case "success", "skipped":
		return scm.ChecksSuccess, true
	case "failed", "canceled":
		return scm.ChecksFailure, true
	case "created", "waiting_for_resource", "preparing", "pending", "running", "scheduled", "manual", "canceling":
		return scm.ChecksPending, true
	}
	return scm.ChecksNone, false
}

// Event maps the pipeline to the neutral event. The check name is the
// pipeline's name, else "pipeline". The host time is finished_at when the
// pipeline has finished, else created_at (the payload has no updated_at).
func (e PipelineEvent) Event() scm.Event {
	name := e.Attrs.Name
	if cleanedEmpty(name) {
		name = defaultCheckName
	}
	ev := scm.Event{
		Provider: ProviderName, Kind: scm.KindChecks,
		RepoKey: strconv.FormatInt(e.Project.ID, 10), RepoPath: e.Project.PathWithNamespace,
		HeadSHA: e.Attrs.SHA, CheckName: name, Checks: scm.ChecksNone, Review: scm.ReviewNone,
		OccurredAt: parseTime(e.Attrs.CreatedAt),
	}
	if t := parseTime(e.Attrs.FinishedAt); !t.IsZero() {
		ev.OccurredAt = t
	}
	if st, ok := pipelineState(e.Attrs.Status); ok {
		ev.Action, ev.Checks = scm.ActionChecks, st
	}
	return ev
}

func cleanedEmpty(s string) bool {
	for _, r := range s {
		if r > ' ' && r != 0x7f {
			return false
		}
	}
	return true
}

// checkKeys are the delivery keys of a pipeline event (decisions-T17 #19):
// the forms of DeliveryKeys with the check suffix instead of the MR one,
//
//	gl:ik:<Idempotency-Key>:p<project id>:sha<first 12 of the head sha>:checks
//	gl:ev:<X-Gitlab-Event-UUID>:p<project id>:sha<first 12>:checks
//
// (GitLab's keys say "p", not "r": the existing form stays one form.)
func checkKeys(idempotencyKey, eventUUID string, projectID int64, ev scm.Event) []string {
	suffix := ":p" + strconv.FormatInt(projectID, 10) + ":sha" + scm.ShortSHA(ev.HeadSHA) + ":checks"
	keys := make([]string, 0, 2)
	if idempotencyKey != "" {
		keys = append(keys, "gl:ik:"+idempotencyKey+suffix)
	}
	return append(keys, "gl:ev:"+eventUUID+suffix)
}
