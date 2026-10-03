package agentpromo

import "sync"

// Store 是驻留内存的 agent 配置晋升库。所有方法并发安全。
type Store struct {
	mu   sync.Mutex
	clk  Clock
	orgs map[string]*orgState
}

type orgState struct {
	policy    Policy
	policySet bool
	agents    map[string]*agentState
	slotsUsed int
}

type agentState struct {
	name     string
	versions map[int64]*Version
	order    []int64
	next     int64 // 下一个版本号，单调递增，永不复用
	pending  int64 // 当前待发布版本号，0 表示无
	deployed int64 // 当前部署版本号，0 表示无
}

// New 创建 Store；clk 为 nil 时使用真实时钟。
func New(clk Clock) *Store {
	if clk == nil {
		clk = realClock{}
	}
	return &Store{clk: clk, orgs: make(map[string]*orgState)}
}

func (s *Store) org(org string) *orgState {
	o, ok := s.orgs[org]
	if !ok {
		o = &orgState{agents: make(map[string]*agentState)}
		s.orgs[org] = o
	}
	return o
}

// SetPolicy 初始化或更新组织策略。每次成功调用产生新策略代次，
// 即使内容相同。尚未部署的 Approved 版本回到 Review 并保留送审时刻；
// 已部署版本不受追溯影响。槽位上限低于当前占用时失败，策略与代次不变。
func (s *Store) SetPolicy(org string, p Policy) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.org(org)
	if o.policySet && p.MaxDeploymentSlots < o.slotsUsed {
		return ErrSlotLimitBelowUsed
	}
	p.Generation = o.policy.Generation + 1
	o.policy = p
	o.policySet = true
	for _, a := range o.agents {
		for _, id := range a.order {
			v := a.versions[id]
			if v.Status == StatusApproved {
				v.Status = StatusReview
				v.Approval = nil
			}
		}
	}
	return nil
}

// NewDraft 创建 agent 或产生新的待发布 Draft 版本，作废此前待发布
// 版本（不影响当前部署）。版本号单调递增，永不复用。
func (s *Store) NewDraft(org, agent string, cfg Config) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.org(org)
	a, ok := o.agents[agent]
	if !ok {
		a = &agentState{name: agent, versions: make(map[int64]*Version), next: 1}
		o.agents[agent] = a
	}
	if a.pending != 0 && a.pending != a.deployed {
		if pv, ok := a.versions[a.pending]; ok {
			switch pv.Status {
			case StatusDraft, StatusReview, StatusApproved:
				pv.Status = StatusRetired
				pv.Approval = nil
			}
		}
	}
	num := a.next
	a.next++
	cp := Config{Tools: append([]string(nil), cfg.Tools...), Prompt: make(map[string]string, len(cfg.Prompt))}
	for k, val := range cfg.Prompt {
		cp.Prompt[k] = val
	}
	a.versions[num] = &Version{Number: num, Config: cp, Status: StatusDraft}
	a.order = append(a.order, num)
	a.pending = num
	return num, nil
}

// Advance 对指定版本执行送审、批准、部署或退役，绝不自动改操作最新版本。
func (s *Store) Advance(org, agent string, version int64, action Action) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orgs[org]
	if !ok {
		return ErrUnknownAgent
	}
	a, ok := o.agents[agent]
	if !ok {
		return ErrUnknownAgent
	}
	v, ok := a.versions[version]
	if !ok {
		return ErrUnknownVersion
	}
	switch action {
	case ActionSubmit:
		return s.submit(o, a, v)
	case ActionApprove:
		return s.approve(o, a, v)
	case ActionDeploy:
		return s.deploy(o, a, v)
	case ActionRetire:
		return s.retire(o, a, v)
	default:
		return ErrInvalidTransition
	}
}

func (s *Store) submit(o *orgState, a *agentState, v *Version) error {
	if v.Status != StatusDraft {
		return ErrInvalidTransition
	}
	v.Status = StatusReview
	v.Submitted = s.clk.Now()
	v.HasSubmit = true
	return nil
}

func (s *Store) approve(o *orgState, a *agentState, v *Version) error {
	if v.Status != StatusReview {
		return ErrInvalidTransition
	}
	if !o.policySet {
		return ErrNoPolicy
	}
	if len(v.Config.Tools) > o.policy.MaxTools {
		return ErrTooManyTools
	}
	for _, k := range o.policy.RequiredPromptKeys {
		if v.Config.Prompt[k] == "" {
			return ErrMissingPromptField
		}
	}
	now := s.clk.Now()
	if now.Sub(v.Submitted) < o.policy.ReviewWait {
		return ErrWaitNotElapsed
	}
	v.Status = StatusApproved
	v.Approval = &Approval{Version: v.Number, PolicyGeneration: o.policy.Generation, ApprovedAt: now}
	return nil
}

func (s *Store) deploy(o *orgState, a *agentState, v *Version) error {
	if v.Status != StatusApproved || a.pending != v.Number {
		return ErrInvalidTransition
	}
	if v.Approval == nil || v.Approval.Version != v.Number ||
		v.Approval.PolicyGeneration != o.policy.Generation {
		return ErrStaleApproval
	}
	if a.deployed == 0 {
		if o.slotsUsed+1 > o.policy.MaxDeploymentSlots {
			return ErrNoSlots
		}
		o.slotsUsed++
	} else {
		old := a.versions[a.deployed]
		old.Status = StatusRetired
		old.Approval = nil
	}
	v.Status = StatusDeployed
	a.deployed = v.Number
	return nil
}

func (s *Store) retire(o *orgState, a *agentState, v *Version) error {
	if v.Status != StatusDeployed || a.deployed != v.Number {
		return ErrInvalidTransition
	}
	v.Status = StatusRetired
	v.Approval = nil
	a.deployed = 0
	o.slotsUsed--
	return nil
}

// Read 返回组织的一致快照。单锁保证不会看到热替换的中间态。
func (s *Store) Read(org string) Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := Snapshot{Org: org}
	o, ok := s.orgs[org]
	if !ok {
		return snap
	}
	snap.Policy = o.policy
	snap.PolicySet = o.policySet
	snap.SlotsUsed = o.slotsUsed
	for _, a := range o.agents {
		av := Agent{Name: a.name, DeployedVersion: a.deployed}
		if a.deployed != 0 {
			av.SlotsUsed = 1
		}
		for _, id := range a.order {
			v := a.versions[id]
			vc := *v
			vc.Config.Tools = append([]string(nil), v.Config.Tools...)
			vc.Config.Prompt = make(map[string]string, len(v.Config.Prompt))
			for k, val := range v.Config.Prompt {
				vc.Config.Prompt[k] = val
			}
			if v.Approval != nil {
				ap := *v.Approval
				vc.Approval = &ap
			}
			av.Versions = append(av.Versions, vc)
		}
		snap.Agents = append(snap.Agents, av)
	}
	return snap
}
