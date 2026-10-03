package agentpromo

import "time"

// Status 表示配置版本的生命周期状态。
type Status string

const (
	StatusDraft    Status = "Draft"
	StatusReview   Status = "Review"
	StatusApproved Status = "Approved"
	StatusDeployed Status = "Deployed"
	StatusRetired  Status = "Retired"
)

// Action 是 Advance 支持的操作。
type Action string

const (
	ActionSubmit  Action = "submit"
	ActionApprove Action = "approve"
	ActionDeploy  Action = "deploy"
	ActionRetire  Action = "retire"
)

// Config 是 agent 配置内容。
type Config struct {
	Tools  []string
	Prompt map[string]string
}

// Policy 是组织策略。每次成功 SetPolicy 产生新代次。
type Policy struct {
	MaxTools            int
	RequiredPromptKeys  []string
	ReviewWait          time.Duration
	MaxDeploymentSlots  int
	Generation          int64
}

// Approval 记录一次有效审批，绑定版本号与策略代次。
type Approval struct {
	Version          int64
	PolicyGeneration int64
	ApprovedAt       time.Time
}

// Version 是 agent 的一个配置版本。
type Version struct {
	Number     int64
	Config     Config
	Status     Status
	Submitted  time.Time
	HasSubmit  bool
	Approval   *Approval
}

// Agent 快照视图。
type Agent struct {
	Name            string
	Versions        []Version
	DeployedVersion int64 // 0 表示无部署
	SlotsUsed       int
}

// Snapshot 是 Read 返回的组织一致快照。
type Snapshot struct {
	Org            string
	Policy         Policy
	PolicySet      bool
	Agents         []Agent
	SlotsUsed      int
}
