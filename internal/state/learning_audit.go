package state

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"q-agent/internal/learning"
)

type LearningAudit struct{ Dir string }

var auditID = regexp.MustCompile(`^[A-Z2-7]{26}$`)

func (a LearningAudit) Append(ctx context.Context, event learning.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := event.Validate(); err != nil {
		return err
	}
	validOutcome := (event.Type == learning.ResearchPlanned && event.Outcome == "dry_run") ||
		(event.Type == learning.ModelCallReserved && event.Outcome == "reserved")
	if !auditID.MatchString(event.ID) || !auditID.MatchString(event.RunID) ||
		!validOutcome {
		return fmt.Errorf("invalid learning audit event metadata")
	}
	for _, ref := range event.ObjectRefs {
		if !auditID.MatchString(ref) {
			return fmt.Errorf("invalid learning audit object reference")
		}
	}
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(a.Dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(a.Dir, ".event-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	dst := filepath.Join(a.Dir, event.ID+".json")
	if err := os.Link(f.Name(), dst); err != nil {
		if !os.IsExist(err) {
			return err
		}
		previous, err := os.ReadFile(dst)
		if err != nil {
			return err
		}
		if !bytes.Equal(data, previous) {
			return learning.ErrVersionConflict
		}
	}
	return SyncDir(a.Dir)
}
