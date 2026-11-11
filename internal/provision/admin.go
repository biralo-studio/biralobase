package provision

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/biralo-studio/biralobase/core"
	"github.com/biralo-studio/biralobase/tools/types"
)

const adminParam = "biralobase_managed_admin"

// ReconcileAdmin atomically keeps one stable identity synchronized with its
// secret, without changing token keys or password hashes on unchanged restarts.
func ReconcileAdmin(app core.App, email, password string) error {
	if email == "" && password == "" {
		return nil
	}
	return app.RunInTransaction(func(tx core.App) error {
		p := &core.Param{}
		err := tx.ModelQuery(p).Model(adminParam, p)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var id string
		if len(p.Value) > 0 {
			if err = json.Unmarshal(p.Value, &id); err != nil {
				return errors.New("invalid managed administrator identity")
			}
		}
		var admin *core.Record
		if id != "" {
			admin, err = tx.FindRecordById(core.CollectionNameSuperusers, id)
			if err != nil {
				return fmt.Errorf("managed administrator is missing; restore it before reconciling: %w", err)
			}
		} else {
			admin, err = tx.FindAuthRecordByEmail(core.CollectionNameSuperusers, email)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if admin != nil {
				return errors.New("configured email belongs to an unmanaged administrator; choose a new managed email")
			}
			collection, err := tx.FindCollectionByNameOrId(core.CollectionNameSuperusers)
			if err != nil {
				return err
			}
			admin = core.NewRecord(collection)
		}
		changed := admin.IsNew() || admin.Email() != email || !admin.ValidatePassword(password)
		if changed {
			admin.SetEmail(email)
			if !admin.ValidatePassword(password) {
				admin.SetPassword(password)
			}
			if err := tx.Save(admin); err != nil {
				return fmt.Errorf("reconcile managed administrator: %w", err)
			}
		}
		if id == "" {
			p.Id = adminParam
			raw, _ := json.Marshal(admin.Id)
			p.Value = types.JSONRaw(raw)
			return tx.Save(p)
		}
		return nil
	})
}
