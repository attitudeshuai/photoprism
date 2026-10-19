package api

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/photoprism/photoprism/internal/auth/acl"
	"github.com/photoprism/photoprism/internal/entity/query"
	"github.com/photoprism/photoprism/internal/event"
	"github.com/photoprism/photoprism/internal/workers"
	"github.com/photoprism/photoprism/pkg/i18n"
	"github.com/photoprism/photoprism/pkg/log/status"
	"github.com/photoprism/photoprism/pkg/txt"
)

// defaultJobRunsLimit is the default number of job runs returned by the ledger endpoint.
const defaultJobRunsLimit = 100

// maxJobRunsLimit caps the number of job runs a single request can return.
const maxJobRunsLimit = 1000

// GetJobs returns the registered background jobs with their latest ledger state.
//
//	@Summary	returns registered background jobs with their latest run state
//	@Id			GetJobs
//	@Tags		Jobs
//	@Produce	json
//	@Success	200				{array}		workers.JobInfo
//	@Failure	401,403,429	{object}	i18n.Response
//	@Router		/api/v1/jobs [get]
func GetJobs(router *gin.RouterGroup) {
	router.GET("/jobs", func(c *gin.Context) {
		// Check authentication and authorization.
		s := Auth(c, acl.ResourceJobs, acl.ActionSearch)

		if s.Abort(c) {
			return
		}

		c.JSON(http.StatusOK, workers.JobInfos())
	})
}

// SearchJobRuns returns the most recent logged runs of a background job as JSON.
//
//	@Summary	returns the most recent logged runs of a background job
//	@Id			SearchJobRuns
//	@Tags		Jobs
//	@Produce	json
//	@Success	200				{object}	entity.JobRun
//	@Header		200				{number}	X-Count		"The actual number of runs returned"
//	@Header		200				{number}	X-Limit		"The limit of the number of runs to be returned"
//	@Header		200				{number}	X-Offset	"The offset that was used"
//	@Failure	401,403,404,429	{object}	i18n.Response
//	@Param		name			path		string	true	"job name"
//	@Param		count			query		int		false	"maximum number of results"	minimum(1)	maximum(1000)
//	@Param		offset			query		int		false	"search result offset"			minimum(0)
//	@Router		/api/v1/jobs/{name}/runs [get]
func SearchJobRuns(router *gin.RouterGroup) {
	router.GET("/jobs/:name/runs", func(c *gin.Context) {
		// Check authentication and authorization.
		s := Auth(c, acl.ResourceJobs, acl.ActionSearch)

		if s.Abort(c) {
			return
		}

		name := c.Param("name")

		if !workers.JobExists(name) {
			Abort(c, http.StatusNotFound, i18n.ErrNotFound, name)
			return
		}

		limit := txt.Int(c.Query("count"))
		offset := txt.Int(c.Query("offset"))

		if limit <= 0 {
			limit = defaultJobRunsLimit
		} else if limit > maxJobRunsLimit {
			limit = maxJobRunsLimit
		}

		if offset < 0 {
			offset = 0
		}

		// Find and return matching job runs.
		resp, err := query.JobRuns(name, limit, offset)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": txt.UpperFirst(err.Error())})
			return
		}

		AddCountHeader(c, len(resp))
		AddLimitHeader(c, limit)
		AddOffsetHeader(c, offset)

		c.JSON(http.StatusOK, resp)
	})
}

// RunJobNow triggers one run of the named background job, using the same ledger and
// retry path as the scheduler, and returns the UID of the new run cycle.
//
//	@Summary	triggers a background job to run now
//	@Id			RunJobNow
//	@Tags		Jobs
//	@Produce	json
//	@Success	202					{object}	map[string]string
//	@Failure	401,403,404,409,429	{object}	i18n.Response
//	@Param		name				path		string	true	"job name"
//	@Router		/api/v1/jobs/{name}/run [post]
func RunJobNow(router *gin.RouterGroup) {
	router.POST("/jobs/:name/run", func(c *gin.Context) {
		// Check authentication and authorization.
		s := Auth(c, acl.ResourceJobs, acl.ActionUpdate)

		if s.Abort(c) {
			return
		}

		name := c.Param("name")

		if !workers.JobExists(name) {
			Abort(c, http.StatusNotFound, i18n.ErrNotFound, name)
			return
		}

		runUID, err := workers.StartJob(name, workers.TriggerManual)
		if err != nil {
			if errors.Is(err, workers.ErrJobAlreadyRunning) {
				Abort(c, http.StatusConflict, i18n.ErrBusy)
				return
			}

			event.AuditErr([]string{ClientIP(c), "session %s", "run background job", name, status.Error(err)}, s.RefID)
			Abort(c, http.StatusInternalServerError, i18n.ErrUnexpected)
			return
		}

		event.AuditInfo([]string{ClientIP(c), "session %s", "run background job", name, "run %s", status.Succeeded}, s.RefID, runUID)

		c.JSON(http.StatusAccepted, gin.H{"status": "ok", "run": runUID})
	})
}
