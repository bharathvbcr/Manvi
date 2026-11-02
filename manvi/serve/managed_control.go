package serve

import (
	"context"
	"sort"
	"time"
)

// What a server that outlives its host needs to know about its managed jobs.
// All of it reads the runner's own table: a job is live from the moment Prepare
// registers it until its drive loop (or its abandoned preparation) closes done.

// liveJobs snapshots the jobs that have not ended, in id order. A nil runner has
// none.
func (r *ManagedRunner) liveJobs() []*managedJob {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	jobs := make([]*managedJob, 0, len(r.jobs))
	for _, j := range r.jobs {
		select {
		case <-j.done:
		default:
			jobs = append(jobs, j)
		}
	}
	r.mu.Unlock()
	sort.Slice(jobs, func(a, b int) bool { return jobs[a].id < jobs[b].id })
	return jobs
}

// LiveJobCount is how many managed jobs have not ended.
func (r *ManagedRunner) LiveJobCount() int { return len(r.liveJobs()) }

// Jobs describes the managed jobs that have not ended. It reads and changes
// nothing, so a host may ask at any time, including before it has decided
// whether it owns what it finds.
func (r *ManagedRunner) Jobs() []ServeJob {
	live := r.liveJobs()
	out := make([]ServeJob, 0, len(live))
	for _, j := range live {
		out = append(out, j.describe())
	}
	return out
}

// describe is the job's entry in serve.status.
func (j *managedJob) describe() ServeJob {
	job := ServeJob{ID: j.id, OwnerID: j.owner, SessionID: j.session}
	j.mu.Lock()
	job.Phase, job.Activated = j.phase, j.activated
	if j.provider != nil {
		job.ProcessID = j.provider.PID()
	}
	j.mu.Unlock()
	// providerName is written by the preparing goroutine without the job's lock
	// and is read only once that goroutine has said it is done with it.
	select {
	case <-j.ready:
		job.Provider = j.providerName
	default:
	}
	if status, err := j.continueStatus(); err == nil {
		job.Continue = &status
	}
	return job
}

// StopAll cancels every managed job that has not ended, the way work.runs.managed.stop
// cancels one, and waits up to wait for them to end. It returns the ids that had
// ended by then and the ids still ending. It does not close the runner: a job
// prepared afterwards runs as usual.
func (r *ManagedRunner) StopAll(ctx context.Context, wait time.Duration) (stopped, pending []string) {
	stopped, pending = []string{}, []string{}
	live := r.liveJobs()
	for _, j := range live {
		j.cancel()
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	expired := false
	for _, j := range live {
		if !expired {
			select {
			case <-j.done:
			case <-timer.C:
				expired = true
			case <-ctx.Done():
				expired = true
			}
		}
		// After the wait, a job is judged by whether it has already ended.
		select {
		case <-j.done:
			stopped = append(stopped, j.id)
		default:
			pending = append(pending, j.id)
		}
	}
	return stopped, pending
}
