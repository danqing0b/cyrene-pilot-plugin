// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"time"
)

const probeInterval = 5 * time.Second
const observationWindow = 60 * time.Second
const approvalLifetime = 30 * time.Minute

type coordinator struct {
	store   *store
	product *products
	now     func() time.Time
	wake    chan struct{}
}

func (c *coordinator) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *coordinator) prepare(candidateID, key string) (*release, error) {
	if !identifier.MatchString(candidateID) || !identifier.MatchString(key) {
		return nil, permanentError("candidateId and idempotencyKey are required identifiers")
	}
	var result *release
	err := c.store.update(func(s *state) error {
		if id := s.Keys[key]; id != "" {
			r := s.Releases[id]
			if r.CandidateID != candidateID {
				return errConflict
			}
			result = r
			return nil
		}
		item, ok := c.product.cfg.Candidates[candidateID]
		if !ok {
			return permanentError("candidate is not registered by the operator")
		}
		if item.Model.Digest == s.Baseline.Digest {
			return errConflict
		}
		if s.JobID != "" || s.Active.ID != s.Baseline.RouteID ||
			s.Active.Alias != s.Baseline.Alias || s.Active.Model != s.Baseline.Model {
			return errConflict
		}
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return err
		}
		r := &release{ID: hex.EncodeToString(id[:]), RequestKey: key, CandidateID: candidateID, Candidate: item, State: queued, Reference: s.Baseline}
		s.JobID, s.Keys[key], s.Releases[r.ID] = r.ID, r.ID, r
		result = r
		return nil
	})
	if err == nil {
		c.signal()
	}
	return result, err
}

func (c *coordinator) edit(id, expected string, fn func(*state, *release) error) error {
	return c.store.update(func(s *state) error {
		r := s.Releases[id]
		if r == nil {
			return errNotFound
		}
		if r.State != expected {
			return errConflict
		}
		return fn(s, r)
	})
}

func (c *coordinator) run(ctx context.Context) {
	clock := time.NewTicker(time.Second)
	defer clock.Stop()
	c.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-clock.C:
			c.tick(ctx)
		case <-c.wake:
			c.tick(ctx)
		}
	}
}

func (c *coordinator) tick(ctx context.Context) {
	s, err := c.store.view()
	if err != nil || ctx.Err() != nil {
		return
	}
	r := s.Releases[s.JobID]
	if r == nil {
		for _, entry := range s.Releases {
			if entry.State == released && entry.Feedback != nil &&
				entry.Feedback.PreparationID == "" && entry.Message == "" &&
				(r == nil || entry.ID < r.ID) {
				r = entry
			}
		}
		if r == nil {
			return
		}
	}
	switch r.State {
	case queued:
		err = c.evaluate(ctx, r)
	case deploying:
		err = c.stage(ctx, s, r)
	case monitoring, verifyingRollback:
		// Health checks use a different clock from tool-triggered work.
		if c.now().Before(r.NextProbe) {
			return
		}
		err = c.observe(ctx, s, r)
	case released:
		if r.Feedback != nil && r.Feedback.PreparationID == "" && r.Message == "" {
			err = c.deliverFeedback(ctx, r)
		}
	}
	if err == nil || errors.Is(err, errWaiting) || errors.Is(err, errConflict) || ctx.Err() != nil {
		return
	}
	_ = c.edit(r.ID, r.State, func(s *state, current *release) error {
		if terminal(err) {
			current.Message = err.Error()
			if current.State != released {
				current.State, s.JobID = rejected, ""
			}
		} else if current.State != released {
			current.Message = "waiting for the downstream service to confirm the operation"
		}
		return nil
	})
}

func (c *coordinator) evaluate(ctx context.Context, r *release) error {
	baseline := r.Baseline
	if baseline.RunID == "" {
		var err error
		cfg := r.Reference
		baseline, err = c.product.evaluate(ctx, cfg.InputID, cfg.Digest, r.ID+"-baseline")
		if err != nil {
			return err
		}
		if err := c.edit(r.ID, queued, func(_ *state, current *release) error {
			current.Baseline, current.Message = baseline, ""
			return nil
		}); err != nil {
			return err
		}
	}
	candidate, err := c.product.evaluate(ctx, r.Candidate.InputID, r.Candidate.Model.Digest, r.ID+"-candidate")
	if err != nil {
		return err
	}
	return c.edit(r.ID, queued, func(s *state, current *release) error {
		current.Evaluation, current.Message = candidate, ""
		if len(candidate.SampleIDs) != 100 || candidate.Dataset != baseline.Dataset ||
			!slices.Equal(candidate.SampleIDs, baseline.SampleIDs) ||
			!candidate.Pass || candidate.Score < 0.90 || candidate.Score < baseline.Score {
			current.State, current.Message, s.JobID = rejected, "candidate did not meet the release gate", ""
		} else {
			current.State = deploying
			c.signal()
		}
		return nil
	})
}

func (c *coordinator) stage(ctx context.Context, s state, r *release) error {
	deployment, err := c.product.deploy(ctx, r)
	if err != nil {
		return err
	}
	if r.DeploymentID == "" {
		if err := c.edit(r.ID, deploying, func(_ *state, current *release) error {
			current.DeploymentID, current.Message = deployment.ID, ""
			return nil
		}); err != nil {
			return err
		}
	}
	if deployment.State == "FAILED" || deployment.State == "STOPPED" || deployment.State == "STOPPING" {
		return permanentError("Reactor deployment terminated")
	}
	if deployment.State != "READY" || !identifier.MatchString(deployment.EndpointID) {
		return errWaiting
	}
	e, err := c.product.endpoint(ctx, deployment.EndpointID, deployment.ID)
	if err != nil {
		return err
	}
	if r.DraftID == "" {
		draft, err := c.product.draft(ctx, r, e)
		if err != nil {
			return err
		}
		if err := c.edit(r.ID, deploying, func(_ *state, current *release) error {
			current.DraftID, current.EndpointID, current.Target = draft.ID, e.ID, draft.target()
			return nil
		}); err != nil {
			return err
		}
		r.DraftID, r.EndpointID, r.Target = draft.ID, e.ID, draft.target()
	}
	target, err := c.product.activate(ctx, r)
	if err != nil {
		return err
	}
	requestID, err := c.product.probe(ctx, target)
	if err != nil {
		return err
	}
	base, err := c.product.readRoute(ctx, s.Active.ID)
	if err != nil {
		return err
	}
	if base.State != "ACTIVE" || base.Alias != s.Active.Alias || base.Model != s.Active.Model ||
		base.Source.Digest != r.Baseline.Digest || base.Endpoint != c.product.cfg.GatewayID {
		return permanentError("baseline route does not match its assessed model")
	}
	if _, err := c.product.probe(ctx, base.target()); err != nil {
		return err
	}
	return c.edit(r.ID, deploying, func(state *state, current *release) error {
		if state.Version != s.Version || state.Active.ID != base.ID {
			return errConflict
		}
		current.State, current.Prior, current.Target = awaitingApproval, base.target(), target
		current.ExpectedVersion, current.LastRequestID, current.Message = s.Version, requestID, ""
		createSnapshot(current, c.now())
		return nil
	})
}

func createSnapshot(r *release, now time.Time) {
	r.ApprovalExpires = now.Add(approvalLifetime)
	data, _ := json.Marshal([]any{r.ID, r.Evaluation.ResultID, r.Candidate.Model.Digest,
		r.Prior, r.Target, r.ExpectedVersion, r.ApprovalExpires})
	sum := sha256.Sum256(data)
	r.Approval = hex.EncodeToString(sum[:])
}

type approval struct {
	Snapshot     string `json:"snapshot"`
	RouteVersion int    `json:"routeVersion"`
}

func (c *coordinator) approve(ctx context.Context, id string, input approval, refresh bool) error {
	s, err := c.store.view()
	if err != nil {
		return err
	}
	r := s.Releases[id]
	if r == nil {
		return errNotFound
	}
	if r.Approval == input.Snapshot && r.ExpectedVersion == input.RouteVersion &&
		r.SwitchedVersion > 0 && !refresh {
		return nil
	}
	if r.State != awaitingApproval || r.Approval != input.Snapshot || r.ExpectedVersion != input.RouteVersion ||
		s.Version != input.RouteVersion || (!refresh && !c.now().Before(r.ApprovalExpires)) {
		return errConflict
	}
	e, err := c.product.endpoint(ctx, r.EndpointID, r.DeploymentID)
	if err != nil {
		return err
	}
	if e.Model != r.Target.Model {
		return errConflict
	}
	target, err := c.product.readRoute(ctx, r.Target.ID)
	if err != nil {
		return err
	}
	base, err := c.product.readRoute(ctx, r.Prior.ID)
	if err != nil {
		return err
	}
	if target.target() != r.Target || target.State != "ACTIVE" || !c.product.routeMatches(target, r) ||
		base.target() != r.Prior || base.State != "ACTIVE" || base.Source.Digest != r.Baseline.Digest {
		return errConflict
	}
	if _, err := c.product.probe(ctx, base.target()); err != nil {
		return err
	}
	requestID, err := c.product.probe(ctx, target.target())
	if err != nil {
		return err
	}
	return c.edit(id, awaitingApproval, func(s *state, current *release) error {
		if s.Version != input.RouteVersion || s.Active.ID != r.Prior.ID || current.Approval != input.Snapshot {
			return errConflict
		}
		now := c.now()
		if refresh {
			createSnapshot(current, now)
			return nil
		}
		if !now.Before(current.ApprovalExpires) {
			return errConflict
		}
		s.Active, s.Version = current.Target, s.Version+1
		current.State, current.SwitchedVersion = monitoring, s.Version
		current.StableSince, current.NextProbe, current.LastRequestID = now, now.Add(probeInterval), requestID
		return nil
	})
}

func (c *coordinator) observe(ctx context.Context, s state, r *release) error {
	requestID, probeErr := c.product.probe(ctx, s.Active)
	return c.edit(r.ID, r.State, func(s *state, current *release) error {
		now := c.now()
		probeWasLate := now.After(current.NextProbe.Add(4 * time.Second))
		current.LastRequestID, current.NextProbe = requestID, now.Add(probeInterval)
		if current.State == verifyingRollback {
			if s.Active != current.Prior || s.Version != current.SwitchedVersion+1 || probeErr != nil {
				current.State, current.Message = needsAttention, "baseline recovery could not be verified"
			} else {
				current.State, s.JobID = rolledBack, ""
			}
			return nil
		}
		if s.Version != current.SwitchedVersion || s.Active != current.Target {
			current.State, current.Message = needsAttention, "route changed during observation"
			return nil
		}
		if probeErr == nil {
			if current.Failures > 0 || probeWasLate {
				current.StableSince = now
			}
			current.Failures = 0
			if now.Sub(current.StableSince) >= observationWindow {
				current.State, s.JobID = released, ""
				s.Baseline = baselineConfig{
					InputID: current.Candidate.InputID, Digest: current.Candidate.Model.Digest,
					RouteID: current.Target.ID, Alias: current.Target.Alias, Model: current.Target.Model,
				}
			}
			return nil
		}
		current.Failures++
		if current.Failures == 3 {
			s.Active, s.Version = current.Prior, s.Version+1
			current.State, current.NextProbe = verifyingRollback, now
			c.signal()
		}
		return nil
	})
}
