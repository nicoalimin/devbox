package job

// BeginDrain serializes against assignment/review/reply admission. Existing
// workers continue; no job is interrupted or relabeled for an upgrade.
func (o *Orchestrator) BeginDrain() {
	o.admissionMu.Lock()
	defer o.admissionMu.Unlock()
	o.maintenance = true
}

func (o *Orchestrator) EndDrain() {
	o.admissionMu.Lock()
	defer o.admissionMu.Unlock()
	o.maintenance = false
}

func (o *Orchestrator) IsDraining() bool {
	o.admissionMu.Lock()
	defer o.admissionMu.Unlock()
	return o.maintenance
}

func (o *Orchestrator) IsDrained() (bool, error) {
	o.activeMu.Lock()
	active := len(o.activeJobs) > 0
	o.activeMu.Unlock()
	if active {
		return false, nil
	}
	jobs, err := o.db.ListJobs(0)
	if err != nil {
		return false, err
	}
	for _, job := range jobs {
		if job.State.IsBusy() {
			return false, nil
		}
		// A blocked session could still be executing in OpenCode. Do not
		// assume that the database state alone proves a safe restart.
		if job.State == "blocked" && job.OpenCodeSessionID != "" {
			busy, err := o.opencode.IsSessionBusy(job.OpenCodeSessionID, job.WorktreePath)
			if err != nil {
				return false, err
			}
			if busy {
				return false, nil
			}
		}
	}
	return true, nil
}
