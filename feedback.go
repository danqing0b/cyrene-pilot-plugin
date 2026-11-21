// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"net/http"
	"slices"
)

type feedbackRequest struct {
	RunID         string   `json:"runId"`
	AnnotationIDs []string `json:"annotationIds"`
}

func (c *coordinator) acceptFeedback(id string, request feedbackRequest) error {
	if !identifier.MatchString(request.RunID) || len(request.AnnotationIDs) == 0 || len(request.AnnotationIDs) > 100 {
		return permanentError("feedback requires one run and up to 100 annotation IDs")
	}
	ids := slices.Clone(request.AnnotationIDs)
	slices.Sort(ids)
	for i, key := range ids {
		if !identifier.MatchString(key) || (i > 0 && ids[i-1] == key) {
			return permanentError("annotation IDs must be valid and unique")
		}
	}
	err := c.edit(id, released, func(_ *state, r *release) error {
		if r.Feedback != nil {
			if r.Feedback.RunID == request.RunID && slices.Equal(r.Feedback.AnnotationIDs, ids) {
				return nil
			}
			return errConflict
		}
		r.Feedback, r.Message = &feedback{RunID: request.RunID, AnnotationIDs: ids}, ""
		return nil
	})
	if err == nil {
		c.signal()
	}
	return err
}

func (c *coordinator) deliverFeedback(ctx context.Context, r *release) error {
	f := r.Feedback
	if f.FeedbackSetID == "" {
		id, err := c.createFeedbackSet(ctx, r)
		if err != nil {
			return err
		}
		if err := c.edit(r.ID, released, func(_ *state, current *release) error {
			current.Feedback.FeedbackSetID = id
			return nil
		}); err != nil {
			return err
		}
		f.FeedbackSetID = id
	}
	base := resource("/api/v1/feedback-sets/", f.FeedbackSetID, "")
	if f.Digest == "" {
		var exported struct {
			Artifact artifact `json:"exportArtifact"`
			Count    int      `json:"candidateCount"`
		}
		if err := c.product.json(ctx, c.product.cfg.Echo, http.MethodPost, base+"/export", "", nil, &exported); err != nil {
			return err
		}
		if exported.Count != len(f.AnnotationIDs) || exported.Artifact.Kind != "dataset" ||
			!digestPattern.MatchString(exported.Artifact.Digest) {
			return permanentError("Echo did not export exactly the reviewed samples")
		}
		if err := c.edit(r.ID, released, func(_ *state, current *release) error {
			current.Feedback.Digest = exported.Artifact.Digest
			return nil
		}); err != nil {
			return err
		}
	}
	var receipt struct {
		Status string
		Target struct {
			ID              string
			URI             string
			ResourceVersion int
		} `json:"targetResource"`
	}
	if err := c.product.json(ctx, c.product.cfg.Echo, http.MethodPost, base+"/actions/send-to-catalyst", "", struct{}{}, &receipt); err != nil {
		return err
	}
	if !identifier.MatchString(receipt.Target.ID) || receipt.Target.URI != "cyrene://catalyst/preparations/"+receipt.Target.ID ||
		receipt.Status != "DRAFT" || receipt.Target.ResourceVersion < 1 {
		return permanentError("invalid Catalyst preparation receipt")
	}
	return c.edit(r.ID, released, func(_ *state, current *release) error {
		current.Feedback.PreparationID = receipt.Target.ID
		return nil
	})
}

func (c *coordinator) createFeedbackSet(ctx context.Context, r *release) (string, error) {
	f := r.Feedback
	path := resource("/api/v1/evaluation-runs/", f.RunID, "")
	var run evaluationRun
	if err := c.product.json(ctx, c.product.cfg.Echo, http.MethodGet, path, "", nil, &run); err != nil {
		return "", err
	}
	if run.ID != f.RunID {
		return "", permanentError("feedback run does not match the request")
	}
	evidence, err := c.product.assess(ctx, run, r.Candidate.Model.Digest)
	if err != nil {
		return "", err
	}
	if evidence.Dataset == r.Evaluation.Dataset {
		return "", permanentError("feedback must use an independent dataset")
	}
	for _, sample := range evidence.SampleIDs {
		if slices.Contains(r.Evaluation.SampleIDs, sample) {
			return "", permanentError("feedback overlaps the release benchmark")
		}
	}
	var annotations []struct {
		ID              string
		RunID           string `json:"runId"`
		SampleIndex     int    `json:"sampleIndex"`
		CorrectedOutput string `json:"correctedOutput"`
		Reviewer        string
	}
	if err := c.product.json(ctx, c.product.cfg.Echo, http.MethodGet, path+"/annotations", "", nil, &annotations); err != nil {
		return "", err
	}
	indexes := make([]int, 0, len(f.AnnotationIDs))
	for _, ann := range annotations {
		if !slices.Contains(f.AnnotationIDs, ann.ID) {
			continue
		}
		if ann.RunID != f.RunID || ann.Reviewer == "" || ann.CorrectedOutput == "" ||
			!slices.Contains(evidence.EvaluatedIndexes, ann.SampleIndex) || slices.Contains(indexes, ann.SampleIndex) {
			return "", permanentError("feedback selection includes an invalid or duplicate correction")
		}
		indexes = append(indexes, ann.SampleIndex)
	}
	if len(indexes) != len(f.AnnotationIDs) {
		return "", permanentError("some selected annotations were not found")
	}
	slices.Sort(indexes)
	var set struct {
		ID    string
		RunID string `json:"runId"`
	}
	err = c.product.json(ctx, c.product.cfg.Echo, http.MethodPost, "/api/v1/feedback-sets", r.ID+"-feedback",
		map[string]any{"name": "release-pilot-" + r.ID, "runId": f.RunID, "annotationIds": f.AnnotationIDs, "sampleIndexes": indexes}, &set)
	if err != nil {
		return "", err
	}
	if !identifier.MatchString(set.ID) || set.RunID != f.RunID {
		return "", permanentError("invalid Echo feedback-set receipt")
	}
	return set.ID, nil
}
