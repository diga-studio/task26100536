package promote

import "time"

// Status 是配置版本的生命周期状态。
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
	ActionSubmit  Action = "Submit"  // 送审
	ActionApprove Action = "Approve" // 批准
	ActionDeploy  Action = "Deploy"  // 部署
	ActionRetire  Action = "Retire"  // 退役
)

// Tool 是单个工具定义。
type Tool struct {
	Name        string
	Description string
}

// Config 是 agent 配置：tools 定义数组 + prompt 字段映射。
type Config struct {
	Tools  []Tool
	Prompt map[string]string
}

// Policy 是组织策略。
type Policy struct {
	MaxTools             int           // 工具数上限
	RequiredPromptFields []string      // 必填 prompt 字段
	ReviewWait           time.Duration // 审查等待时长
	MaxDeploySlots       int           // 部署槽位上限
}

// Approval 记录一次批准，绑定版本号与批准时的策略代次。
type Approval struct {
	Version    int
	PolicyGen  int
	ApprovedAt time.Time
}

// Version 是 agent 的一个配置版本。
type Version struct {
	Number      int
	Config      Config
	Status      Status
	SubmittedAt time.Time // 送审时刻，未送审为零值
	Approval    *Approval // 当前有效审批，无则 nil
}

// AgentSnapshot 是单个 agent 的一致快照。
type AgentSnapshot struct {
	Versions []Version // 按版本号升序
	Deployed int       // 当前部署版本号，0 表示无
	Pending  int       // 当前待发布版本号，0 表示无
}

// Snapshot 是组织的一致快照。
type Snapshot struct {
	Policy         Policy
	PolicyGen      int
	Agents         map[string]AgentSnapshot
	SlotsUsed      int // 当前部署占用槽位数
	MaxDeploySlots int
}
