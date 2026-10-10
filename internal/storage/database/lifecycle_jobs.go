package database

// The jobs domain: the background jobs' run state (migration 0041, ADR 0023), one row per declared job.
func init() {
	registerLifecycle(lifecycleDomain{
		name: "jobs",
		tables: []tableClaim{
			{table: "job_runs", noExport: "background job bookkeeping, no user's data",
				erase: notPersonal, eraseWhy: "background job bookkeeping: job names, run times, the replica that ran them and the last error",
				org: orgNotKeyed, noPurge: "one row per declared job, overwritten by each run"},
		},
	})
}
