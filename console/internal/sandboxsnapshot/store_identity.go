package sandboxsnapshot

import (
	"context"
	"errors"
)

// RequiresStore prevents a feature toggle from silently abandoning accepted
// operations or committed data. Native-only deletion does not require S3.
func (r *Repository) RequiresStore(ctx context.Context) (bool, error) {
	var required bool
	err := r.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sandbox_snapshots WHERE status!='deleted') OR EXISTS(SELECT 1 FROM sandbox_persistence_operations WHERE status IN ('pending','running') OR (status='failed' AND phase='reconciling'))`).Scan(&required)
	return required, err
}
func (r *Repository) ValidateStore(ctx context.Context, id string) error {
	var mismatched bool
	if err := r.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sandbox_snapshots WHERE status!='deleted' AND COALESCE(json_extract(upload_json,'$.object.store_id'),'')!=?)`, id).Scan(&mismatched); err != nil {
		return err
	}
	if mismatched {
		return errors.New("snapshot store identity differs from retained snapshot references; migrate stored objects and references before changing the namespace")
	}
	return nil
}
