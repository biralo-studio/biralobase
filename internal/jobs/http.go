package jobs

import (
	"github.com/biralo-studio/biralobase/apis"
	"github.com/biralo-studio/biralobase/core"
	"github.com/biralo-studio/biralobase/tools/router"
	"github.com/pocketbase/dbx"
)

func (q *Queue) BindHTTP(routes *router.Router[*core.RequestEvent]) {
	group := routes.Group("/api/biralobase/jobs").Bind(apis.RequireSuperuserAuth())
	group.GET("", func(e *core.RequestEvent) error {
		status := e.Request.URL.Query().Get("status")
		switch status {
		case "pending", "running", "dead", "done":
		case "":
			status = "dead"
		default:
			return e.BadRequestError("Unknown job status", nil)
		}
		var rows []struct {
			ID        string `db:"id" json:"id"`
			Kind      string `json:"kind"`
			Target    string `json:"target"`
			Status    string `json:"status"`
			Attempts  int    `json:"attempts"`
			Created   int64  `json:"created"`
			LastError string `db:"last_error" json:"lastError"`
		}
		if err := q.app.DB().NewQuery("SELECT id,kind,target,status,attempts,created,last_error FROM _bb_jobs WHERE status={:status} ORDER BY created DESC,id LIMIT 100").Bind(dbx.Params{"status": status}).All(&rows); err != nil {
			return err
		}
		return e.JSON(200, rows)
	})
	group.POST("/{id}/retry", func(e *core.RequestEvent) error {
		result, err := q.app.DB().NewQuery("UPDATE _bb_jobs SET status='pending',attempts=0,available_at=0,last_error='' WHERE id={:id} AND status='dead'").Bind(dbx.Params{"id": e.Request.PathValue("id")}).Execute()
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return e.NotFoundError("Dead job not found", nil)
		}
		return e.NoContent(204)
	})
}
