package agentpromo

import (
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func testPolicy() Policy {
	return Policy{
		MaxTools:           3,
		RequiredPromptKeys: []string{"system"},
		ReviewWait:         time.Hour,
		MaxDeploymentSlots: 2,
	}
}

func goodConfig() Config {
	return Config{Tools: []string{"search"}, Prompt: map[string]string{"system": "you are helpful"}}
}

func mustSetPolicy(t *testing.T, s *Store, org string, p Policy) {
	t.Helper()
	if err := s.SetPolicy(org, p); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}
}

func mustDraft(t *testing.T, s *Store, org, agent string, cfg Config) int64 {
	t.Helper()
	v, err := s.NewDraft(org, agent, cfg)
	if err != nil {
		t.Fatalf("NewDraft: %v", err)
	}
	return v
}

func mustAdvance(t *testing.T, s *Store, org, agent string, v int64, a Action) {
	t.Helper()
	if err := s.Advance(org, agent, v, a); err != nil {
		t.Fatalf("Advance(%s): %v", a, err)
	}
}

// 等待期边界：差一纳秒失败，恰好到达成功。
func TestReviewWaitBoundary(t *testing.T) {
	clk := newFakeClock()
	s := New(clk)
	mustSetPolicy(t, s, "o1", testPolicy())
	v := mustDraft(t, s, "o1", "a1", goodConfig())
	mustAdvance(t, s, "o1", "a1", v, ActionSubmit)

	clk.Advance(time.Hour - time.Nanosecond)
	if err := s.Advance("o1", "a1", v, ActionApprove); !errors.Is(err, ErrWaitNotElapsed) {
		t.Fatalf("approve before wait: got %v, want ErrWaitNotElapsed", err)
	}
	// 失败后状态保持 Review，无审批残留。
	snap := s.Read("o1")
	got := snap.Agents[0].Versions[0]
	if got.Status != StatusReview || got.Approval != nil {
		t.Fatalf("failed approve left partial state: %+v", got)
	}

	clk.Advance(time.Nanosecond)
	mustAdvance(t, s, "o1", "a1", v, ActionApprove)
	snap = s.Read("o1")
	got = snap.Agents[0].Versions[0]
	if got.Status != StatusApproved || got.Approval == nil {
		t.Fatalf("approve at boundary: %+v", got)
	}
	if got.Approval.PolicyGeneration != snap.Policy.Generation {
		t.Fatalf("approval gen %d != policy gen %d", got.Approval.PolicyGeneration, snap.Policy.Generation)
	}
}

// 策略更新使未部署 Approved 回到 Review（保留送审时刻），重新等待后可再批准。
func TestPolicyUpdateInvalidatesApproval(t *testing.T) {
	clk := newFakeClock()
	s := New(clk)
	mustSetPolicy(t, s, "o1", testPolicy())
	v := mustDraft(t, s, "o1", "a1", goodConfig())
	mustAdvance(t, s, "o1", "a1", v, ActionSubmit)
	submittedAt := clk.Now()
	clk.Advance(time.Hour)
	mustAdvance(t, s, "o1", "a1", v, ActionApprove)

	// 内容相同的策略也产生新代次。
	p := testPolicy()
	mustSetPolicy(t, s, "o1", p)
	snap := s.Read("o1")
	if snap.Policy.Generation != 2 {
		t.Fatalf("generation = %d, want 2", snap.Policy.Generation)
	}
	got := snap.Agents[0].Versions[0]
	if got.Status != StatusReview || got.Approval != nil {
		t.Fatalf("approved version should revert to Review: %+v", got)
	}
	if !got.Submitted.Equal(submittedAt) {
		t.Fatalf("submit time lost: %v != %v", got.Submitted, submittedAt)
	}
	// 旧审批失效：直接部署被拒绝。
	if err := s.Advance("o1", "a1", v, ActionDeploy); err == nil {
		t.Fatal("deploy with invalidated approval should fail")
	}
	// 送审时刻保留，等待期自此起算已满足，可立即重新批准。
	mustAdvance(t, s, "o1", "a1", v, ActionApprove)
	snap = s.Read("o1")
	ap := snap.Agents[0].Versions[0].Approval
	if ap == nil || ap.PolicyGeneration != 2 {
		t.Fatalf("re-approval should bind gen 2: %+v", ap)
	}
	mustAdvance(t, s, "o1", "a1", v, ActionDeploy)
}

// 只剩一槽时两个 agent 并发部署，恰有一个成功，失败者保留状态与审批。
func TestConcurrentDeploySingleSlot(t *testing.T) {
	clk := newFakeClock()
	s := New(clk)
	p := testPolicy()
	p.MaxDeploymentSlots = 1
	p.ReviewWait = 0
	mustSetPolicy(t, s, "o1", p)

	versions := make([]int64, 2)
	agents := []string{"a1", "a2"}
	for i, ag := range agents {
		versions[i] = mustDraft(t, s, "o1", ag, goodConfig())
		mustAdvance(t, s, "o1", ag, versions[i], ActionSubmit)
		mustAdvance(t, s, "o1", ag, versions[i], ActionApprove)
	}

	const attempts = 16
	var wg sync.WaitGroup
	results := make([][]error, 2)
	for i := range agents {
		results[i] = make([]error, attempts)
	}
	for i, ag := range agents {
		i, ag := i, ag
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < attempts; j++ {
				results[i][j] = s.Advance("o1", ag, versions[i], ActionDeploy)
			}
		}()
	}
	wg.Wait()

	// 恰有一个 agent 成功部署一次，另一个全部因无槽失败。
	var wins [2]int
	for i := range agents {
		for _, err := range results[i] {
			if err == nil {
				wins[i]++
			}
		}
	}
	if !((wins[0] == 1 && wins[1] == 0) || (wins[1] == 1 && wins[0] == 0)) {
		t.Fatalf("deploy outcomes: wins=%v", wins)
	}
	loser := 0
	if wins[0] == 1 {
		loser = 1
	}
	for _, err := range results[loser] {
		if !errors.Is(err, ErrNoSlots) {
			t.Fatalf("loser err = %v, want ErrNoSlots", err)
		}
	}

	snap := s.Read("o1")
	if snap.SlotsUsed != 1 {
		t.Fatalf("slots used = %d, want 1", snap.SlotsUsed)
	}
	deployed := 0
	for _, ag := range snap.Agents {
		for _, v := range ag.Versions {
			if v.Status == StatusDeployed {
				deployed++
			}
		}
		if ag.Name == agents[loser] {
			lv := ag.Versions[len(ag.Versions)-1]
			if lv.Status != StatusApproved || lv.Approval == nil {
				t.Fatalf("loser state not preserved: %+v", lv)
			}
			if lv.Approval.PolicyGeneration != snap.Policy.Generation {
				t.Fatalf("loser approval gen = %d, want %d",
					lv.Approval.PolicyGeneration, snap.Policy.Generation)
			}
		}
	}
	if deployed != 1 {
		t.Fatalf("deployed versions = %d, want 1", deployed)
	}
}

// 热替换：新版与旧版一次性替换，占用仍为一槽，快照无双部署或空窗。
func TestHotSwapKeepsSingleSlot(t *testing.T) {
	clk := newFakeClock()
	s := New(clk)
	p := testPolicy()
	p.MaxDeploymentSlots = 1
	p.ReviewWait = 0
	mustSetPolicy(t, s, "o1", p)

	v1 := mustDraft(t, s, "o1", "a1", goodConfig())
	mustAdvance(t, s, "o1", "a1", v1, ActionSubmit)
	mustAdvance(t, s, "o1", "a1", v1, ActionApprove)
	mustAdvance(t, s, "o1", "a1", v1, ActionDeploy)

	v2 := mustDraft(t, s, "o1", "a1", goodConfig())
	mustAdvance(t, s, "o1", "a1", v2, ActionSubmit)
	mustAdvance(t, s, "o1", "a1", v2, ActionApprove)
	mustAdvance(t, s, "o1", "a1", v2, ActionDeploy)

	snap := s.Read("o1")
	if snap.SlotsUsed != 1 {
		t.Fatalf("slots used = %d, want 1", snap.SlotsUsed)
	}
	ag := snap.Agents[0]
	if ag.DeployedVersion != v2 {
		t.Fatalf("deployed = %d, want %d", ag.DeployedVersion, v2)
	}
	if ag.Versions[0].Status != StatusRetired || ag.Versions[1].Status != StatusDeployed {
		t.Fatalf("statuses: %v, %v", ag.Versions[0].Status, ag.Versions[1].Status)
	}
	// 迟到的旧版本退役不得影响新部署。
	if err := s.Advance("o1", "a1", v1, ActionRetire); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("late retire: %v", err)
	}
	snap = s.Read("o1")
	if snap.Agents[0].DeployedVersion != v2 || snap.SlotsUsed != 1 {
		t.Fatalf("late retire disturbed deployment: %+v", snap.Agents[0])
	}
	// 退役当前部署版本释放槽位。
	mustAdvance(t, s, "o1", "a1", v2, ActionRetire)
	snap = s.Read("o1")
	if snap.SlotsUsed != 0 || snap.Agents[0].DeployedVersion != 0 {
		t.Fatalf("retire should free slot: %+v", snap)
	}
}

// 槽位上限降到当前占用以下失败，策略与代次不变。
func TestSlotLimitReductionFails(t *testing.T) {
	clk := newFakeClock()
	s := New(clk)
	p := testPolicy()
	p.MaxDeploymentSlots = 1
	p.ReviewWait = 0
	mustSetPolicy(t, s, "o1", p)
	v := mustDraft(t, s, "o1", "a1", goodConfig())
	mustAdvance(t, s, "o1", "a1", v, ActionSubmit)
	mustAdvance(t, s, "o1", "a1", v, ActionApprove)
	mustAdvance(t, s, "o1", "a1", v, ActionDeploy)

	bad := p
	bad.MaxDeploymentSlots = 0
	if err := s.SetPolicy("o1", bad); !errors.Is(err, ErrSlotLimitBelowUsed) {
		t.Fatalf("SetPolicy: %v", err)
	}
	snap := s.Read("o1")
	if snap.Policy.Generation != 1 || snap.Policy.MaxDeploymentSlots != 1 {
		t.Fatalf("policy changed after failed update: %+v", snap.Policy)
	}
	// 已部署版本不受策略更新追溯影响。
	ok := p
	ok.MaxTools = 0
	mustSetPolicy(t, s, "o1", ok)
	snap = s.Read("o1")
	if snap.Agents[0].Versions[0].Status != StatusDeployed {
		t.Fatalf("deployed version retroactively affected: %+v", snap.Agents[0].Versions[0])
	}
}

// 版本号不复用；相同内容再次提交是新版本；NewDraft 作废此前待发布版本。
func TestVersionNumbersNeverReused(t *testing.T) {
	clk := newFakeClock()
	s := New(clk)
	mustSetPolicy(t, s, "o1", testPolicy())
	v1 := mustDraft(t, s, "o1", "a1", goodConfig())
	v2 := mustDraft(t, s, "o1", "a1", goodConfig())
	if v2 != v1+1 {
		t.Fatalf("versions %d, %d", v1, v2)
	}
	snap := s.Read("o1")
	if snap.Agents[0].Versions[0].Status != StatusRetired {
		t.Fatalf("superseded draft should be retired: %+v", snap.Agents[0].Versions[0])
	}
	// Advance 不得自动改操作最新版本：对作废旧版本送审失败。
	if err := s.Advance("o1", "a1", v1, ActionSubmit); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("submit superseded draft: %v", err)
	}
}

// 批准校验：工具数超限与必填 prompt 字段缺失。
func TestApproveValidation(t *testing.T) {
	clk := newFakeClock()
	s := New(clk)
	p := testPolicy()
	p.ReviewWait = 0
	mustSetPolicy(t, s, "o1", p)

	v1 := mustDraft(t, s, "o1", "a1", Config{
		Tools:  []string{"a", "b", "c", "d"},
		Prompt: map[string]string{"system": "x"},
	})
	mustAdvance(t, s, "o1", "a1", v1, ActionSubmit)
	if err := s.Advance("o1", "a1", v1, ActionApprove); !errors.Is(err, ErrTooManyTools) {
		t.Fatalf("approve too many tools: %v", err)
	}

	v2 := mustDraft(t, s, "o1", "a2", Config{
		Tools:  []string{"a"},
		Prompt: map[string]string{"system": ""},
	})
	mustAdvance(t, s, "o1", "a2", v2, ActionSubmit)
	if err := s.Advance("o1", "a2", v2, ActionApprove); !errors.Is(err, ErrMissingPromptField) {
		t.Fatalf("approve missing prompt: %v", err)
	}
	snap := s.Read("o1")
	for _, ag := range snap.Agents {
		if ag.Versions[0].Status != StatusReview || ag.Versions[0].Approval != nil {
			t.Fatalf("failed approve left partial state: %+v", ag.Versions[0])
		}
	}
}
