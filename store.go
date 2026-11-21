// SPDX-License-Identifier: Apache-2.0
package main

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"
)

const (
	queued            = "QUEUED"
	deploying         = "DEPLOYING"
	awaitingApproval  = "AWAITING_APPROVAL"
	monitoring        = "MONITORING"
	released          = "RELEASED"
	verifyingRollback = "VERIFYING_ROLLBACK"
	rolledBack        = "ROLLED_BACK"
	rejected          = "REJECTED"
	needsAttention    = "NEEDS_ATTENTION"
)

var errConflict = errors.New("resource or revision conflict")
var errNotFound = errors.New("release not found")
var bucket = []byte("release-pilot-v1")
var rootKey = []byte("state")

type route struct {
	ID      string `json:"id"`
	Alias   string `json:"alias"`
	Model   string `json:"model"`
	Version int    `json:"resourceVersion"`
}

type assessment struct {
	RunID            string   `json:"runId"`
	GateID           string   `json:"gateId"`
	ResultID         string   `json:"resultId"`
	Dataset          string   `json:"dataset"`
	Digest           string   `json:"digest"`
	SampleIDs        []string `json:"sampleIds"`
	EvaluatedIndexes []int    `json:"evaluatedIndexes"`
	Score            float64  `json:"score"`
	Pass             bool     `json:"pass"`
}

type release struct {
	ID              string         `json:"id"`
	RequestKey      string         `json:"requestKey"`
	CandidateID     string         `json:"candidateId"`
	Candidate       candidate      `json:"candidate"`
	State           string         `json:"state"`
	Reference       baselineConfig `json:"baselineConfig"`
	Baseline        assessment     `json:"baselineAssessment"`
	Evaluation      assessment     `json:"evaluation"`
	DeploymentID    string         `json:"deploymentId,omitempty"`
	EndpointID      string         `json:"endpointId,omitempty"`
	DraftID         string         `json:"draftId,omitempty"`
	Target          route          `json:"target"`
	Prior           route          `json:"prior"`
	ExpectedVersion int            `json:"expectedRouteVersion,omitempty"`
	SwitchedVersion int            `json:"switchedRouteVersion,omitempty"`
	Approval        string         `json:"approvalSnapshot,omitempty"`
	ApprovalExpires time.Time      `json:"approvalExpiresAt,omitempty"`
	StableSince     time.Time      `json:"stableSince,omitempty"`
	NextProbe       time.Time      `json:"nextProbeAt,omitempty"`
	Failures        int            `json:"probeFailures,omitempty"`
	LastRequestID   string         `json:"lastProbeRequestId,omitempty"`
	Message         string         `json:"message,omitempty"`
	Feedback        *feedback      `json:"feedback,omitempty"`
}

type feedback struct {
	RunID         string   `json:"runId"`
	AnnotationIDs []string `json:"annotationIds"`
	FeedbackSetID string   `json:"feedbackSetId,omitempty"`
	Digest        string   `json:"digest,omitempty"`
	PreparationID string   `json:"preparationId,omitempty"`
}

type state struct {
	Active   route               `json:"active"`
	Baseline baselineConfig      `json:"baseline"`
	Version  int                 `json:"routeVersion"`
	JobID    string              `json:"jobId"`
	Releases map[string]*release `json:"releases"`
	Keys     map[string]string   `json:"keys"`
}

type store struct{ db *bolt.DB }

func openStore(directory string, baseline baselineConfig) (*store, error) {
	db, err := bolt.Open(filepath.Join(directory, "release-pilot.db"), 0600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, err
	}
	err = db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(bucket)
		if err != nil || b.Get(rootKey) != nil {
			return err
		}
		initial := state{Active: route{ID: baseline.RouteID, Alias: baseline.Alias, Model: baseline.Model},
			Baseline: baseline, Version: 1, Releases: map[string]*release{}, Keys: map[string]string{}}
		return saveState(b, &initial)
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return &store{db}, nil
}

func saveState(b *bolt.Bucket, s *state) error {
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return b.Put(rootKey, raw)
}

func (s *store) view() (state, error) {
	var value state
	err := s.db.View(func(tx *bolt.Tx) error { return json.Unmarshal(tx.Bucket(bucket).Get(rootKey), &value) })
	return value, err
}

func (s *store) update(fn func(*state) error) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucket)
		var value state
		if err := json.Unmarshal(b.Get(rootKey), &value); err != nil {
			return err
		}
		if err := fn(&value); err != nil {
			return err
		}
		return saveState(b, &value)
	})
}
