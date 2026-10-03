package promote

import (
	"errors"
	"sort"
	"sync"
	"time"
)

var (
	ErrNoPolicy        = errors.New("promote: organization has no policy")
	ErrOrgNotFound     = errors.New("promote: organization not found")
	ErrAgentNotFound   = errors.New("promote: agent not found")
	ErrVersionNotFound = errors.New("promote: version not found")
	ErrBadTransition   = errors.New("promote: action not allowed for current status")
	ErrNotPending      = errors.New("promote: version is not the current pending version")
	ErrPolicyStale     = errors.New("promote: approval policy generation is stale")
	ErrNoSlots         = errors.New("promote: no deployment slots available")
	ErrSlotLimit       = errors.New("promote: slot limit below current occupancy")
	ErrNotDeployed     = errors.New("promote: version is not the current deployment")
	ErrPolicyViolation = errors.New("promote: config violates current policy")
	ErrWaitNotElapsed  = errors.New("promote: review wait duration not elapsed")
)

// Store 是内存中的配置晋升库。时钟可注入，假定时间不倒退。
type Store struct {
	mu   sync.Mutex
	now  func() time.Time
	orgs map[string]*orgState
}

type orgState struct {
	policy    Policy
	policyGen int
	agents    map[string]*agentState
	slotsUsed int
}

type agentState struct {
	versions    map[int]*Version
	nextVersion int
	deployed    int
	pending     int
}

// New 创建 Store；now 为 nil 时使用真实时钟。
func New(now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{now: now, orgs: make(map[string]*orgState)}
}

// SetPolicy 初始化或更新组织策略，每次成功调用产生新策略代次。
func (s *Store) SetPolicy(org string, p Policy) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	o := s.orgs[org]
	if o == nil {
		o = &orgState{agents: make(map[string]*agentState)}
		s.orgs[org] = o
	}
	// 槽位上限不得低于当前占用，失败时策略与代次均不变。
	if o.slotsUsed > 0 && p.MaxDeploySlots < o.slotsUsed {
		return ErrSlotLimit
	}
	o.policy = p
	o.policyGen++
	// 策略更新使尚未部署的 Approved 版本回到 Review，保留送审时刻，
	// 已部署版本不受追溯影响。
	for _, a := range o.agents {
		for _, aVer := range a.versions {
			if aVer.Status == StatusApproved && a.deployed != aVer.Number {
				aVer.Status = StatusReview
				aVer.Approval = nil
			}
		}
	}
	return nil
}

// NewDraft 创建 agent 或产生新的待发布 Draft 版本，返回新版本号。
func (s *Store) NewDraft(org, agent string, cfg Config) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	o := s.orgs[org]
	if o == nil {
		return 0, ErrNoPolicy
	}
	// agent 不存在时创建。
	a := o.agents[agent]
	if a == nil {
		a = &agentState{versions: make(map[int]*Version)}
		o.agents[agent] = a
	}
	// 作废此前待发布版本，不影响当前部署。
	if a.pending != 0 {
		if old := a.versions[a.pending]; old != nil {
			old.Status = StatusRetired
			old.Approval = nil
		}
		a.pending = 0
	}
	// 版本号单调递增，绝不复用。
	a.nextVersion++
	num := a.nextVersion
	a.versions[num] = &Version{Number: num, Config: cfg, Status: StatusDraft}
	a.pending = num
	return num, nil
}

// Advance 对指定版本执行送审、批准、部署或退役。
func (s *Store) Advance(org, agent string, version int, action Action) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	o := s.orgs[org]
	if o == nil {
		return ErrOrgNotFound
	}
	a := o.agents[agent]
	if a == nil {
		return ErrAgentNotFound
	}
	v := a.versions[version]
	if v == nil {
		return ErrVersionNotFound
	}
	switch action {
	case ActionSubmit:
		return s.submit(v)
	case ActionApprove:
		return s.approve(o, v)
	case ActionDeploy:
		return s.deploy(o, a, v)
	case ActionRetire:
		return s.retire(o, a, v)
	}
	return ErrBadTransition
}

func (s *Store) submit(v *Version) error {
	if v.Status != StatusDraft {
		return ErrBadTransition
	}
	v.Status = StatusReview
	v.SubmittedAt = s.now()
	return nil
}

func (s *Store) approve(o *orgState, v *Version) error {
	if v.Status != StatusReview {
		return ErrBadTransition
	}
	if err := checkPolicy(o.policy, v.Config); err != nil {
		return err
	}
	if s.now().Sub(v.SubmittedAt) < o.policy.ReviewWait {
		return ErrWaitNotElapsed
	}
	v.Status = StatusApproved
	v.Approval = &Approval{
		Version:    v.Number,
		PolicyGen:  o.policyGen,
		ApprovedAt: s.now(),
	}
	return nil
}

func checkPolicy(p Policy, cfg Config) error {
	if len(cfg.Tools) > p.MaxTools {
		return ErrPolicyViolation
	}
	for _, field := range p.RequiredPromptFields {
		if cfg.Prompt[field] == "" {
			return ErrPolicyViolation
		}
	}
	return nil
}

func (s *Store) deploy(o *orgState, a *agentState, v *Version) error {
	if v.Status != StatusApproved {
		return ErrBadTransition
	}
	// 只允许部署当前待发布版本，不得自动改操作最新版本。
	if a.pending != v.Number {
		return ErrNotPending
	}
	if v.Approval == nil || v.Approval.Version != v.Number || v.Approval.PolicyGen != o.policyGen {
		return ErrPolicyStale
	}
	// 原子替换：已有部署时新旧版本一次性替换，槽位占用不变。
	if a.deployed != 0 {
		old := a.versions[a.deployed]
		old.Status = StatusRetired
		v.Status = StatusDeployed
		a.deployed = v.Number
		a.pending = 0
		return nil
	}
	// 首次部署需占用一个槽位。
	if o.slotsUsed >= o.policy.MaxDeploySlots {
		return ErrNoSlots
	}
	o.slotsUsed++
	v.Status = StatusDeployed
	a.deployed = v.Number
	a.pending = 0
	return nil
}

func (s *Store) retire(o *orgState, a *agentState, v *Version) error {
	// 只允许退役当前部署版本；迟到的旧版本退役不影响后来部署。
	if a.deployed != v.Number || v.Status != StatusDeployed {
		return ErrNotDeployed
	}
	v.Status = StatusRetired
	a.deployed = 0
	o.slotsUsed--
	return nil
}

// Read 返回组织的一致快照。
func (s *Store) Read(org string) (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	o := s.orgs[org]
	if o == nil {
		return Snapshot{}, ErrOrgNotFound
	}
	snap := Snapshot{
		Policy:         o.policy,
		PolicyGen:      o.policyGen,
		Agents:         make(map[string]AgentSnapshot, len(o.agents)),
		SlotsUsed:      o.slotsUsed,
		MaxDeploySlots: o.policy.MaxDeploySlots,
	}
	for name, a := range o.agents {
		as := AgentSnapshot{Deployed: a.deployed, Pending: a.pending}
		nums := make([]int, 0, len(a.versions))
		for n := range a.versions {
			nums = append(nums, n)
		}
		sort.Ints(nums)
		for _, n := range nums {
			v := a.versions[n]
			cp := *v
			if v.Approval != nil {
				ap := *v.Approval
				cp.Approval = &ap
			}
			as.Versions = append(as.Versions, cp)
		}
		snap.Agents[name] = as
	}
	return snap, nil
}
