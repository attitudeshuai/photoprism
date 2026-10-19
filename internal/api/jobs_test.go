package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/photoprism/photoprism/internal/workers"
)

// waitForJobRunCount polls the ledger endpoint until the job has the expected number of rows.
func waitForJobRunCount(t *testing.T, app *gin.Engine, name string, count int) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		r := PerformRequest(app, "GET", "/api/v1/jobs/"+name+"/runs")
		require.Equal(t, http.StatusOK, r.Code)

		var rows []map[string]any
		require.NoError(t, json.Unmarshal(r.Body.Bytes(), &rows))

		if len(rows) >= count {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("job %s did not record %d runs", name, count)
}

func TestGetJobs(t *testing.T) {
	t.Run("Success", func(t *testing.T) {
		const name = "test-api-jobs"
		require.NoError(t, workers.RegisterJob(name, func() error { return nil }))

		app, router, _ := NewApiTest()
		GetJobs(router)

		r := PerformRequest(app, "GET", "/api/v1/jobs")
		require.Equal(t, http.StatusOK, r.Code)

		var jobs []workers.JobInfo
		require.NoError(t, json.Unmarshal(r.Body.Bytes(), &jobs))

		found := false
		for _, job := range jobs {
			if job.Name == name {
				found = true
			}
		}

		assert.True(t, found)
	})
}

func TestSearchJobRuns(t *testing.T) {
	t.Run("UnknownJob", func(t *testing.T) {
		app, router, _ := NewApiTest()
		SearchJobRuns(router)

		r := PerformRequest(app, "GET", "/api/v1/jobs/test-api-unknown/runs")
		assert.Equal(t, http.StatusNotFound, r.Code)
	})
	t.Run("Success", func(t *testing.T) {
		const name = "test-api-job-runs"
		require.NoError(t, workers.RegisterJob(name, func() error { return nil }))

		app, router, _ := NewApiTest()
		SearchJobRuns(router)
		RunJobNow(router)

		r := PerformRequest(app, "POST", "/api/v1/jobs/"+name+"/run")
		require.Equal(t, http.StatusAccepted, r.Code)

		var resp map[string]string
		require.NoError(t, json.Unmarshal(r.Body.Bytes(), &resp))
		assert.NotEmpty(t, resp["run"])

		waitForJobRunCount(t, app, name, 1)

		r = PerformRequest(app, "GET", "/api/v1/jobs/"+name+"/runs?count=10")
		require.Equal(t, http.StatusOK, r.Code)
		assert.Equal(t, "10", r.Header().Get("X-Limit"))

		var rows []map[string]any
		require.NoError(t, json.Unmarshal(r.Body.Bytes(), &rows))
		require.Len(t, rows, 1)
		assert.Equal(t, "succeeded", rows[0]["Status"])
		assert.Equal(t, "manual", rows[0]["Trigger"])
		assert.Empty(t, rows[0]["Error"])
	})
}

func TestRunJobNow(t *testing.T) {
	t.Run("UnknownJob", func(t *testing.T) {
		app, router, _ := NewApiTest()
		RunJobNow(router)

		r := PerformRequest(app, "POST", "/api/v1/jobs/test-api-unknown/run")
		assert.Equal(t, http.StatusNotFound, r.Code)
	})
	t.Run("AlreadyRunning", func(t *testing.T) {
		const name = "test-api-job-busy"
		started := make(chan struct{})
		release := make(chan struct{})
		require.NoError(t, workers.RegisterJob(name, func() error {
			close(started)
			<-release
			return nil
		}))
		t.Cleanup(func() {
			select {
			case <-release:
			default:
				close(release)
			}
		})

		app, router, _ := NewApiTest()
		RunJobNow(router)

		r := PerformRequest(app, "POST", "/api/v1/jobs/"+name+"/run")
		require.Equal(t, http.StatusAccepted, r.Code)
		<-started

		r = PerformRequest(app, "POST", "/api/v1/jobs/"+name+"/run")
		assert.Equal(t, http.StatusConflict, r.Code)

		close(release)
	})
}
