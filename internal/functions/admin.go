package functions

import "github.com/biralo-studio/biralobase/core"

// Admin makes construction available alongside the transaction-scoped Go App.
// Application code must opt into ctx.admin; ctx.api retains caller permissions.
type Admin struct{ core.App }

func (a *Admin) NewRecord(collection string) (*core.Record, error) {
	c, err := a.FindCachedCollectionByNameOrId(collection)
	if err != nil {
		return nil, err
	}
	return core.NewRecord(c), nil
}
