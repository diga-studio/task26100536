package promote

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeClock 是可注入的手动时钟。
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func testPolicy() Policy {
	return Policy{
		MaxTools:             2,
		RequiredPromptFields: []string{"system"},
		ReviewWait:           10 * time.Minute,
		MaxDeploySlots:       2,
	}
}

func goodConfig() Config {
	return Config{
		Tools:  []Tool{{Name: "search"}},
		Prompt: map[string]string{"system": "you are helpful"},
	}
}

func mustAdvance(t *testing.T, s *Store, org, agent string, v int, a Action) {
	t.Helper()
	if err := s.Advance(org, agent, v, a); err != nil {
		t.Fatalf("Advance(%v) on v%d: %v", a, v, err)
	}
}

func findVersion(t *testing.T, snap Snapshot, agent string, num int) Version {
	t.Helper()
	for _, v := range snap.Agents[agent].Versions {
		if v.Number == num {
			return v
		}
	}
	t.Fatalf("version %d of agent %s not found", num, agent)
	return Version{}
}

// TestReviewWaitBoundary 用注入时钟验证审查等待期边界。
func TestReviewWaitBoundary(t *testing.T) {
	clk := newFakeClock()
	s := New(clk.now)
	if err := s.SetPolicy("org1", testPolicy()); err != nil {
		t.Fatal(err)
	}
	v, err := s.NewDraft("org1", "agent1", goodConfig())
	if err != nil {
		t.Fatal(err)
	}
	mustAdvance(t, s, "org1", "agent1", v, ActionSubmit)

	// 等待期未满：批准失败，且不留下部分变化。
	clk.add(9 * time.Minute)
	if err := s.Advance("org1", "agent1", v, ActionApprove); !errors.Is(err, ErrWaitNotElapsed) {
		t.Fatalf("approve before wait: got %v, want ErrWaitNotElapsed", err)
	}
	snap, _ := s.Read("org1")
	v1 := findVersion(t, snap, "agent1", v)
	if v1.Status != StatusReview || v1.Approval != nil {
		t.Fatalf("failed approve left partial state: status=%s approval=%v", v1.Status, v1.Approval)
	}

	// 恰好到达等待期边界：批准成功。
	clk.add(1 * time.Minute)
	mustAdvance(t, s, "org1", "agent1", v, ActionApprove)
	snap, _ = s.Read("org1")
	v1 = findVersion(t, snap, "agent1", v)
	if v1.Status != StatusApproved {
		t.Fatalf("status = %s, want Approved", v1.Status)
	}
	if v1.Approval == nil || v1.Approval.Version != v || v1.Approval.PolicyGen != 1 {
		t.Fatalf("approval binding wrong: %+v", v1.Approval)
	}
}

// TestPolicyUpdateInvalidatesApproval 策略更新使未部署审批失效后重新审批。
func TestPolicyUpdateInvalidatesApproval(t *testing.T) {
	clk := newFakeClock()
	s := New(clk.now)
	p := testPolicy()
	s.SetPolicy("org1", p)
	v, _ := s.NewDraft("org1", "a1", goodConfig())
	mustAdvance(t, s, "org1", "a1", v, ActionSubmit)
	clk.add(10 * time.Minute)
	mustAdvance(t, s, "org1", "a1", v, ActionApprove)

	// 策略更新（即使内容相同也产生新代次），未部署 Approved 回到 Review。
	snap0, _ := s.Read("org1")
	s.SetPolicy("org1", p)
	snap, _ := s.Read("org1")
	if snap.PolicyGen != snap0.PolicyGen+1 {
		t.Fatalf("policy gen not bumped: %d -> %d", snap0.PolicyGen, snap.PolicyGen)
	}
	v1 := findVersion(t, snap, "a1", v)
	if v1.Status != StatusReview || v1.Approval != nil {
		t.Fatalf("approved version not reverted: status=%s approval=%v", v1.Status, v1.Approval)
	}
	// 保留原送审时刻：等待期已过，可直接重新批准。
	mustAdvance(t, s, "org1", "a1", v, ActionApprove)
	snap, _ = s.Read("org1")
	v1 = findVersion(t, snap, "a1", v)
	if v1.Approval == nil || v1.Approval.PolicyGen != snap.PolicyGen {
		t.Fatalf("re-approval bound to wrong gen: %+v", v1.Approval)
	}
	// 重新批准后可部署。
	mustAdvance(t, s, "org1", "a1", v, ActionDeploy)
	snap, _ = s.Read("org1")
	if snap.Agents["a1"].Deployed != v || snap.SlotsUsed != 1 {
		t.Fatalf("deploy failed: deployed=%d slots=%d", snap.Agents["a1"].Deployed, snap.SlotsUsed)
	}
}

// TestConcurrentDeploySingleSlot 只剩一槽时两个 agent 并发部署，恰有一个成功。
func TestConcurrentDeploySingleSlot(t *testing.T) {
	clk := newFakeClock()
	s := New(clk.now)
	p := testPolicy()
	p.MaxDeploySlots = 1
	s.SetPolicy("org1", p)

	ready := func(agent string) int {
		v, err := s.NewDraft("org1", agent, goodConfig())
		if err != nil {
			t.Fatal(err)
		}
		mustAdvance(t, s, "org1", agent, v, ActionSubmit)
		clk.add(10 * time.Minute)
		mustAdvance(t, s, "org1", agent, v, ActionApprove)
		return v
	}
	v1 := ready("a1")
	v2 := ready("a2")

	// 并发部署，恰有一个成功。
	var wg sync.WaitGroup
	results := make([]error, 2)
	for i, agent := range []string{"a1", "a2"} {
		wg.Add(1)
		go func(i int, agent string) {
			defer wg.Done()
			v := v1
			if agent == "a2" {
				v = v2
			}
			results[i] = s.Advance("org1", agent, v, ActionDeploy)
		}(i, agent)
	}
	wg.Wait()

	var succeeded, failed int
	for _, err := range results {
		if err == nil {
			succeeded++
		} else if errors.Is(err, ErrNoSlots) {
			failed++
		} else {
			t.Fatalf("unexpected deploy error: %v", err)
		}
	}
	if succeeded != 1 || failed != 1 {
		t.Fatalf("succeeded=%d failed=%d, want 1/1", succeeded, failed)
	}

	// 恰有一个部署，槽位占用为 1。
	snap, _ := s.Read("org1")
	if snap.SlotsUsed != 1 {
		t.Fatalf("slots used = %d, want 1", snap.SlotsUsed)
	}
	// 失败者保留原状态与审批。
	for _, agent := range []string{"a1", "a2"} {
		v := v1
		if agent == "a2" {
			v = v2
		}
		v1Snap := findVersion(t, snap, agent, v)
		if v1Snap.Status == StatusApproved {
			if v1Snap.Approval == nil || v1Snap.Approval.Version != v {
				t.Fatalf("loser %s lost approval: %+v", agent, v1Snap.Approval)
			}
		} else if v1Snap.Status != StatusDeployed {
			t.Fatalf("agent %s version status = %s", agent, v1Snap.Status)
		}
	}
}

// setupApproved 走完 draft->submit->approve 流程，返回版本号。
func setupApproved(t *testing.T, s *Store, clk *fakeClock, org, agent string, cfg Config) int {
	t.Helper()
	v, err := s.NewDraft(org, agent, cfg)
	if err != nil {
		t.Fatal(err)
	}
	mustAdvance(t, s, org, agent, v, ActionSubmit)
	clk.add(10 * time.Minute)
	mustAdvance(t, s, org, agent, v, ActionApprove)
	return v
}

// TestHotSwapDeploy 已有部署时新版一次性替换，占用仍为一槽。
func TestHotSwapDeploy(t *testing.T) {
	clk := newFakeClock()
	s := New(clk.now)
	s.SetPolicy("org1", testPolicy())
	v1 := setupApproved(t, s, clk, "org1", "a1", goodConfig())
	mustAdvance(t, s, "org1", "a1", v1, ActionDeploy)

	v2 := setupApproved(t, s, clk, "org1", "a1", goodConfig())
	mustAdvance(t, s, "org1", "a1", v2, ActionDeploy)

	snap, _ := s.Read("org1")
	if snap.Agents["a1"].Deployed != v2 || snap.SlotsUsed != 1 {
		t.Fatalf("deployed=%d slots=%d, want %d/1", snap.Agents["a1"].Deployed, snap.SlotsUsed, v2)
	}
	if findVersion(t, snap, "a1", v1).Status != StatusRetired {
		t.Fatal("old version not retired")
	}
	// 快照中恰有一个 Deployed，无双部署或空窗。
	deployed := 0
	for _, v := range snap.Agents["a1"].Versions {
		if v.Status == StatusDeployed {
			deployed++
		}
	}
	if deployed != 1 {
		t.Fatalf("deployed count = %d, want 1", deployed)
	}
}

// TestRetireRules 退役只允许当前部署版本，迟到退役不影响后来部署。
func TestRetireRules(t *testing.T) {
	clk := newFakeClock()
	s := New(clk.now)
	s.SetPolicy("org1", testPolicy())
	v1 := setupApproved(t, s, clk, "org1", "a1", goodConfig())
	mustAdvance(t, s, "org1", "a1", v1, ActionDeploy)
	v2 := setupApproved(t, s, clk, "org1", "a1", goodConfig())
	mustAdvance(t, s, "org1", "a1", v2, ActionDeploy)

	// 迟到旧版本退役失败，不影响当前部署。
	if err := s.Advance("org1", "a1", v1, ActionRetire); !errors.Is(err, ErrNotDeployed) {
		t.Fatalf("late retire: got %v, want ErrNotDeployed", err)
	}
	// 未部署版本退役失败。
	v3, _ := s.NewDraft("org1", "a1", goodConfig())
	if err := s.Advance("org1", "a1", v3, ActionRetire); !errors.Is(err, ErrNotDeployed) {
		t.Fatalf("retire draft: got %v, want ErrNotDeployed", err)
	}
	// 当前部署版本退役成功并释放槽位。
	mustAdvance(t, s, "org1", "a1", v2, ActionRetire)
	snap, _ := s.Read("org1")
	if snap.Agents["a1"].Deployed != 0 || snap.SlotsUsed != 0 {
		t.Fatalf("deployed=%d slots=%d, want 0/0", snap.Agents["a1"].Deployed, snap.SlotsUsed)
	}
}

// TestVersionNumbersNotReused 相同内容再次提交也是新版本号。
func TestVersionNumbersNotReused(t *testing.T) {
	clk := newFakeClock()
	s := New(clk.now)
	s.SetPolicy("org1", testPolicy())
	v1, _ := s.NewDraft("org1", "a1", goodConfig())
	v2, _ := s.NewDraft("org1", "a1", goodConfig())
	if v2 <= v1 {
		t.Fatalf("version reused: %d -> %d", v1, v2)
	}
	snap, _ := s.Read("org1")
	// 此前待发布版本被作废。
	if findVersion(t, snap, "a1", v1).Status != StatusRetired {
		t.Fatal("previous pending version not voided")
	}
	if snap.Agents["a1"].Pending != v2 {
		t.Fatal("pending not updated")
	}
}

// TestSlotLimitReduction 降低槽位上限到当前占用以下必须失败，代次不变。
func TestSlotLimitReduction(t *testing.T) {
	clk := newFakeClock()
	s := New(clk.now)
	p := testPolicy()
	p.MaxDeploySlots = 1
	s.SetPolicy("org1", p)
	v := setupApproved(t, s, clk, "org1", "a1", goodConfig())
	mustAdvance(t, s, "org1", "a1", v, ActionDeploy)
	snap0, _ := s.Read("org1")

	bad := p
	bad.MaxDeploySlots = 0
	if err := s.SetPolicy("org1", bad); !errors.Is(err, ErrSlotLimit) {
		t.Fatalf("got %v, want ErrSlotLimit", err)
	}
	snap, _ := s.Read("org1")
	if snap.PolicyGen != snap0.PolicyGen || snap.MaxDeploySlots != 1 {
		t.Fatalf("policy changed on failure: gen %d->%d slots=%d",
			snap0.PolicyGen, snap.PolicyGen, snap.MaxDeploySlots)
	}
}

// TestBadTransitions 状态机非法跳转。
func TestBadTransitions(t *testing.T) {
	clk := newFakeClock()
	s := New(clk.now)
	s.SetPolicy("org1", testPolicy())
	v, _ := s.NewDraft("org1", "a1", goodConfig())
	// Draft 直接批准/部署均失败。
	if err := s.Advance("org1", "a1", v, ActionApprove); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("approve draft: got %v", err)
	}
	if err := s.Advance("org1", "a1", v, ActionDeploy); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("deploy draft: got %v", err)
	}
	mustAdvance(t, s, "org1", "a1", v, ActionSubmit)
	// 重复送审失败。
	if err := s.Advance("org1", "a1", v, ActionSubmit); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("re-submit: got %v", err)
	}
}

// TestPolicyCompliance 工具数与必填 prompt 字段校验。
func TestPolicyCompliance(t *testing.T) {
	clk := newFakeClock()
	s := New(clk.now)
	s.SetPolicy("org1", testPolicy())
	// 工具数超限。
	v, _ := s.NewDraft("org1", "a1", Config{
		Tools:  []Tool{{}, {}, {}},
		Prompt: map[string]string{"system": "x"},
	})
	mustAdvance(t, s, "org1", "a1", v, ActionSubmit)
	clk.add(10 * time.Minute)
	if err := s.Advance("org1", "a1", v, ActionApprove); !errors.Is(err, ErrPolicyViolation) {
		t.Fatalf("too many tools: got %v", err)
	}
	// 必填字段为空字符串。
	v2, _ := s.NewDraft("org1", "a2", Config{
		Tools:  []Tool{{}},
		Prompt: map[string]string{"system": ""},
	})
	mustAdvance(t, s, "org1", "a2", v2, ActionSubmit)
	clk.add(10 * time.Minute)
	if err := s.Advance("org1", "a2", v2, ActionApprove); !errors.Is(err, ErrPolicyViolation) {
		t.Fatalf("empty required field: got %v", err)
	}
}

// TestStaleApprovalCannotDeploy 审批代次过期后部署失败。
func TestStaleApprovalCannotDeploy(t *testing.T) {
	clk := newFakeClock()
	s := New(clk.now)
	s.SetPolicy("org1", testPolicy())
	v := setupApproved(t, s, clk, "org1", "a1", goodConfig())
	// 策略更新使审批回到 Review；直接部署失败。
	s.SetPolicy("org1", testPolicy())
	if err := s.Advance("org1", "a1", v, ActionDeploy); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("deploy reverted version: got %v", err)
	}
}
