# promote

纯 Go 标准库实现的 agent 配置晋升库：配置送审、批准与部署槽位管理，数据驻留内存，时钟可注入（假定时间不倒退）。

## 核心概念

- **版本状态机**：`Draft → Review → Approved → Deployed`，终态 `Retired`。
- **审批绑定**：批准（Approved）时记录绑定的版本号与当前策略代次；部署时校验二者均有效。
- `SetPolicy` 每次成功调用产生新策略代次，并使未部署的 Approved 版本回到 Review（保留送审时刻）。
- 部署原子占用组织唯一槽位：首次部署占一槽，热替换新旧版本一次性切换、占用不变，槽位总数不超过组织上限。
- 版本号单调递增，绝不复用。

## API

```go
s := promote.New(clk.now) // clk.now 可注入时钟；传 nil 用真实时钟

s.SetPolicy("org", promote.Policy{
    MaxTools:             8,
    RequiredPromptFields: []string{"system"},
    ReviewWait:           10 * time.Minute,
    MaxDeploySlots:       2,
})

v, _ := s.NewDraft("org", "agent1", promote.Config{
    Tools:  []promote.Tool{{Name: "search"}},
    Prompt: map[string]string{"system": "you are helpful"},
})
s.Advance("org", "agent1", v, promote.ActionSubmit)  // 送审，记录送审时刻
// ... 等待 ReviewWait 时长 ...
s.Advance("org", "agent1", v, promote.ActionApprove) // 批准，绑定当前策略代次
s.Advance("org", "agent1", v, promote.ActionDeploy)  // 原子取得部署槽位
snap, _ := s.Read("org")                             // 一致快照
```

## 错误语义

| 错误 | 含义 |
|---|---|
| `ErrBadTransition` | 当前状态不允许该操作 |
| `ErrWaitNotElapsed` | 审查等待期未满 |
| `ErrPolicyViolation` | 工具数超限或必填 prompt 字段为空 |
| `ErrPolicyStale` | 审批绑定的策略代次已失效 |
| `ErrNotPending` | 部署目标不是当前待发布版本 |
| `ErrNoSlots` | 组织槽位已满 |
| `ErrSlotLimit` | 槽位上限不能降到当前占用以下 |
| `ErrNotDeployed` | 退役目标不是当前部署版本 |

所有失败操作均为原子失败，不留下状态、审批、送审时刻或槽位的部分变化。

## 测试

```sh
go test ./...        # 全部测试
go test -race ./...   # 含并发槽位竞争的竞态检测
```
