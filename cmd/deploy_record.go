package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/LeonTing1010/mole/utils"
)

var ErrDeployInterrupted = errors.New("deployment interrupted")

// No goroutine deletes instances. Work and its child processes stop first;
// the main flow then waits for the deletion receipt before returning.
func runDeployLifecycle(ctx context.Context, work, cleanup func() error) error {
	err := work()
	if errors.Is(ctx.Err(), context.Canceled) {
		return errors.Join(ErrDeployInterrupted, err, cleanup())
	}
	return err
}

type deploymentRecord struct {
	ID           string            `json:"deploy_id"`
	Name         string            `json:"name"`
	InstanceID   string            `json:"instance_id,omitempty"`
	ScriptID     string            `json:"script_id,omitempty"`
	ScriptSHA256 string            `json:"script_sha256"`
	Stage        string            `json:"stage"`
	UpdatedAt    string            `json:"updated_at"`
	Verification *nodeVerification `json:"verification,omitempty"`
	Error        string            `json:"error,omitempty"`
}

type deploySession struct {
	record deploymentRecord
	dir    string
	log    *os.File
}

func newDeploySession(srv Server, hash string) (*deploySession, error) {
	if !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(srv.DeployID) {
		return nil, fmt.Errorf("invalid deployment ID")
	}
	dir := filepath.Join(utils.MoleDir(), "deployments", srv.DeployID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	log, err := os.OpenFile(filepath.Join(dir, "deploy.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	s := &deploySession{dir: dir, log: log, record: deploymentRecord{ID: srv.DeployID, Name: srv.Name, ScriptSHA256: hash}}
	if data, rerr := os.ReadFile(filepath.Join(dir, "receipt.json")); rerr == nil {
		var old deploymentRecord
		if json.Unmarshal(data, &old) != nil || old.ID != srv.DeployID || old.Name != srv.Name {
			log.Close()
			return nil, fmt.Errorf("existing deployment receipt is inconsistent")
		}
		if hash != "" && old.ScriptSHA256 != "" && hash != old.ScriptSHA256 {
			log.Close()
			return nil, fmt.Errorf("deployment ID has different script content")
		}
		s.record = old
	} else if !errors.Is(rerr, os.ErrNotExist) {
		log.Close()
		return nil, rerr
	}
	// Necessary client-side identity only; never log it. This survives an
	// ambiguous instance POST even when no instance ID was received.
	if err = writePrivateJSON(filepath.Join(dir, "identity.json"), srv); err != nil {
		log.Close()
		return nil, err
	}
	if err = s.stage("prepared", nil); err != nil {
		log.Close()
		return nil, err
	}
	return s, nil
}

func (s *deploySession) writer() io.Writer { return io.MultiWriter(os.Stdout, s.log) }
func (s *deploySession) stage(stage string, err error) error {
	s.record.Stage = stage
	s.record.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	s.record.Error = ""
	if err != nil {
		// Provider responses can echo a submitted script. Store the status,
		// not response bodies, auth material, URIs or generated credentials.
		var se *ServerError
		if errors.As(err, &se) {
			s.record.Error = fmt.Sprintf("provider HTTP %d", se.Status)
		} else {
			s.record.Error = "operation failed; see phase and verification verdicts"
		}
	}
	fmt.Fprintf(s.writer(), "[%s] deployment %s: %s\n", s.record.UpdatedAt, s.record.ID, stage)
	return writePrivateJSON(filepath.Join(s.dir, "receipt.json"), s.record)
}

func (s *deploySession) cleanupInstance(srv Server) error {
	if srv.InstanceID == "" {
		return nil
	}
	stageErr := s.stage("destroying_after_interrupt", nil)
	_, err := serverAPIWithin(apiBudget, "DELETE", "/instances/"+srv.InstanceID, nil)
	if err != nil && !IsServerNotFound(err) {
		_ = s.stage("cleanup_pending", err)
		return fmt.Errorf("instance %s deletion unconfirmed; still potentially billing; retry mole server destroy %s: %w", srv.InstanceID, srv.Name, err)
	}
	if err = removeRecordedServer(srv.InstanceID, srv.Name); err != nil {
		_ = s.stage("deleted_local_cleanup_pending", err)
		return err
	}
	return errors.Join(stageErr, s.stage("destroyed", nil))
}
