package job

import "testing"

func TestUpgradeDrainBlocksAdmissionAndWaitsForWorkers(t *testing.T) {
	orch, job, _ := deliveryFixture(t)
	orch.BeginDrain()
	if _, err := orch.CreateJob("NEW-1", ""); err == nil {
		t.Fatal("assignment accepted during drain")
	}
	if _, err := orch.acceptReview(job.ID, "review"); err == nil {
		t.Fatal("review accepted during drain")
	}
	if err := orch.ReplyToJob(job.ID, "reply"); err == nil {
		t.Fatal("reply accepted during drain")
	}
	orch.markJobActive(job.ID)
	if ready, err := orch.IsDrained(); ready || err != nil {
		t.Fatalf("active worker considered drained: %v %v", ready, err)
	}
	orch.markJobInactive(job.ID)
	if ready, err := orch.IsDrained(); !ready || err != nil {
		t.Fatalf("idle server not drained: %v %v", ready, err)
	}
	orch.EndDrain()
	if orch.IsDraining() {
		t.Fatal("drain did not clear")
	}
}
