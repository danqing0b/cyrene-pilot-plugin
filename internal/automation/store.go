// SPDX-License-Identifier: Apache-2.0
package automation

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

var jobsBucket = []byte("pilot-jobs-v2")
var keysBucket = []byte("pilot-keys-v2")
var ErrConflict = errors.New("idempotency key is bound to different arguments")
var ErrNotFound = errors.New("job or artifact not found")

type Step struct {
	State  string          `json:"state"`
	Result json.RawMessage `json:"result,omitempty"`
	Code   string          `json:"code,omitempty"`
}
type Artifact struct {
	Digest string `json:"digest"`
	Size   int    `json:"sizeBytes"`
}
type Job struct {
	ID            string              `json:"id"`
	Kind          string              `json:"kind"`
	State         string              `json:"state"`
	ConfigDigest  string              `json:"configDigest"`
	RequestDigest string              `json:"requestDigest"`
	Args          json.RawMessage     `json:"args,omitempty"`
	Summary       map[string]any      `json:"summary"`
	Artifacts     map[string]Artifact `json:"artifacts"`
	Steps         map[string]Step     `json:"steps"`
	CreatedAt     time.Time           `json:"createdAt"`
	UpdatedAt     time.Time           `json:"updatedAt"`
}
type Service struct {
	cfg Config
	db  *bolt.DB
	dir string
	mu  sync.Mutex // serializes workers and review-triggered writes, never network work in a DB transaction
}

func Open(dir string, cfg Config) (*Service, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "artifacts"), 0700); err != nil {
		return nil, err
	}
	db, err := bolt.Open(filepath.Join(dir, "automation.db"), 0600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, err
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{jobsBucket, keysBucket} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	s := &Service{cfg: cfg, db: db, dir: dir}
	// An interrupted provider dispatch has an ambiguous outcome. It is never replayed.
	err = db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(jobsBucket)
		var interrupted []*Job
		if err := b.ForEach(func(k, v []byte) error {
			var j Job
			if err := json.Unmarshal(v, &j); err != nil {
				return err
			}
			if j.State == "RUNNING" {
				j.State = "QUEUED"
				interrupted = append(interrupted, &j)
			}
			return nil
		}); err != nil {
			return err
		}
		for _, j := range interrupted {
			if err := putJob(b, j); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}
func (s *Service) Close() error { return s.db.Close() }
func putJob(b *bolt.Bucket, j *Job) error {
	j.UpdatedAt = time.Now().UTC()
	raw, err := json.Marshal(j)
	if err != nil {
		return err
	}
	return b.Put([]byte(j.ID), raw)
}
func (s *Service) save(j *Job) error {
	return s.db.Update(func(tx *bolt.Tx) error { return putJob(tx.Bucket(jobsBucket), j) })
}
func (s *Service) load(id string) (*Job, error) {
	var j Job
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(jobsBucket).Get([]byte(id))
		if b == nil {
			return ErrNotFound
		}
		return json.Unmarshal(b, &j)
	})
	return &j, err
}
func (s *Service) Status(id string) (*Job, error) {
	j, err := s.load(id)
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for key, step := range j.Steps {
		if key == "guard-salt" {
			delete(j.Steps, key)
			continue
		}
		counts[step.State]++
		if step.State == "DONE" && (strings.HasPrefix(key, "answer:") || strings.HasPrefix(key, "guard-ai:")) {
			delete(j.Steps, key)
			continue
		}
		step.Result = nil
		j.Steps[key] = step
	}
	j.Summary["stepCounts"] = counts
	j.Args = nil
	return j, nil
}

func (s *Service) enqueue(kind, key string, args any) (*Job, error) {
	if !ID.MatchString(key) {
		return nil, errors.New("idempotencyKey must be a bounded identifier")
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	hash := Digest(append([]byte(kind+"\n"), raw...))
	var j *Job
	err = s.db.Update(func(tx *bolt.Tx) error {
		keys, b := tx.Bucket(keysBucket), tx.Bucket(jobsBucket)
		if id := keys.Get([]byte(kind + ":" + key)); id != nil {
			var prior Job
			if err := json.Unmarshal(b.Get(id), &prior); err != nil {
				return err
			}
			if prior.RequestDigest != hash || prior.ConfigDigest != s.cfg.fingerprint() {
				return ErrConflict
			}
			j = &prior
			return nil
		}
		if b.Stats().KeyN >= s.cfg.MaxJobs {
			return errors.New("job ledger full; archive completed jobs before increasing capacity")
		}
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return err
		}
		j = &Job{ID: hex.EncodeToString(id[:]), Kind: kind, State: "QUEUED", ConfigDigest: s.cfg.fingerprint(), RequestDigest: hash, Args: raw, Summary: map[string]any{}, Steps: map[string]Step{}, Artifacts: map[string]Artifact{}, CreatedAt: time.Now().UTC()}
		if err := putJob(b, j); err != nil {
			return err
		}
		return keys.Put([]byte(kind+":"+key), []byte(j.ID))
	})
	if err != nil {
		return nil, err
	}
	return s.Status(j.ID)
}

func (s *Service) jobs() ([]*Job, error) {
	var out []*Job
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(jobsBucket).ForEach(func(_, raw []byte) error {
			var j Job
			if err := json.Unmarshal(raw, &j); err != nil {
				return err
			}
			out = append(out, &j)
			return nil
		})
	})
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, err
}

func writePrivate(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".pilot-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, path)
}
func (s *Service) artifact(j *Job, name string, data []byte) error {
	if !ID.MatchString(name) || len(data) > MaxBytes {
		return errors.New("invalid artifact")
	}
	dir := filepath.Join(s.dir, "artifacts", j.ID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if err := writePrivate(filepath.Join(dir, name), data); err != nil {
		return err
	}
	j.Artifacts[name] = Artifact{Digest: Digest(data), Size: len(data)}
	return s.save(j)
}
func (s *Service) ReadArtifact(id, name string) ([]byte, error) {
	j, err := s.load(id)
	if err != nil {
		return nil, err
	}
	a, ok := j.Artifacts[name]
	if !ok || !ID.MatchString(name) {
		return nil, ErrNotFound
	}
	b, err := os.ReadFile(filepath.Join(s.dir, "artifacts", id, name))
	if err != nil {
		return nil, err
	}
	if Digest(b) != a.Digest {
		return nil, errors.New("artifact integrity failure")
	}
	return b, nil
}
