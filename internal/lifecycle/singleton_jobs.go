package lifecycle

import (
	"fmt"
	"hash/fnv"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// JobDecl declares a background job: its name and its cross-replica lock key, once, at package level
// (DeclareJob). The registry is reviewed through internal/lifecycle/testdata/jobs.golden.
type JobDecl struct {
	// Name is the job's identity: the metric and log label, the job_runs row, and the source of a derived key.
	// Lowercase letters, digits and '_', at most 64 characters.
	Name string
	// LegacyKey is the advisory-lock key of a job that shipped before the registry. It must never change: during a
	// rolling deploy the old and the new revision would each hold "the" lock under a different key and run the job
	// side by side. 0 derives the key from the name (a new job).
	LegacyKey int64
	// Schedule says when the job runs, for the golden ("every PAD_SCAN_INTERVAL", "once per boot", "on demand").
	Schedule string
}

// JobSpec is a declared job; DeclareJob builds it.
type JobSpec struct {
	name     string
	key      int64
	legacy   bool
	schedule string
}

// Name returns the declared name.
func (s *JobSpec) Name() string { return s.name }

// Key returns the job's advisory-lock key.
func (s *JobSpec) Key() int64 { return s.key }

// JobInfo is one registry entry, for the golden and the key tests.
type JobInfo struct {
	Name     string
	Key      int64
	Legacy   bool
	Schedule string
}

var (
	jobsMu sync.Mutex
	jobs   = map[string]*JobSpec{}
)

var jobNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// DeclareJob registers a job. Call it as the value of a package-level var, with a literal name, so every job exists
// once its package is linked (the golden test links them all). A malformed name, an empty schedule, or a name or key
// another job has panics at init, so the boot and every test of the package fail.
func DeclareJob(d JobDecl) *JobSpec {
	if !jobNameRE.MatchString(d.Name) {
		panic(fmt.Sprintf("lifecycle.DeclareJob: job name %q must be lowercase letters, digits and '_' (at most 64)", d.Name))
	}
	if strings.TrimSpace(d.Schedule) == "" {
		panic(fmt.Sprintf("lifecycle.DeclareJob: job %q needs a schedule", d.Name))
	}
	spec := &JobSpec{name: d.Name, key: d.LegacyKey, legacy: d.LegacyKey != 0, schedule: d.Schedule}
	if !spec.legacy {
		spec.key = DerivedJobKey(d.Name)
	}
	jobsMu.Lock()
	defer jobsMu.Unlock()
	if _, dup := jobs[d.Name]; dup {
		panic(fmt.Sprintf("lifecycle.DeclareJob: job %q declared twice", d.Name))
	}
	for _, other := range jobs {
		if other.key == spec.key {
			panic(fmt.Sprintf("lifecycle.DeclareJob: job %q has the lock key %#x of job %q", d.Name, spec.key, other.name))
		}
	}
	jobs[d.Name] = spec
	return spec
}

// DerivedJobKey is the advisory-lock key of a job declared without a legacy key: FNV-1a-64 of "baki:job:"+name.
func DerivedJobKey(name string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("baki:job:" + name))
	return int64(h.Sum64()) //nolint:gosec // G115: the hash's bits reinterpreted as the signed advisory-lock key; wraparound is intended
}

// Jobs returns every declared job, by name.
func Jobs() []JobInfo {
	jobsMu.Lock()
	defer jobsMu.Unlock()
	out := make([]JobInfo, 0, len(jobs))
	for _, s := range jobs {
		out = append(out, JobInfo{Name: s.name, Key: s.key, Legacy: s.legacy, Schedule: s.schedule})
	}
	slices.SortFunc(out, func(a, b JobInfo) int { return strings.Compare(a.Name, b.Name) })
	return out
}
