# agentpromo

Go 标准库实现的 agent 配置晋升库：配置送审、批准与部署槽位管理，数据驻留内存，时钟可注入（假定时间不倒退）。

## 核心概念

- **Policy**：组织策略（工具数上限、必填 prompt 字段、审查等待时长、部署槽位上限）。每次成功的 `SetPolicy` 产生新策略代次，即使内容相同。
- **Version**：agent 的配置版本，状态机 `Draft → Review → Approved → Deployed → Retired`。版本号按 agent 单调递增，永不复用。
- **Approval**：批准时生成，绑定版本号与当前策略代次；策略更新使未部署的 Approved 版本回到 Review（保留送审时刻），已部署版本不受追溯。
- **Slot**：首次部署占一个组织槽位；热替换时新旧版本原子交换，占用不变；退役当前部署版本释放槽位。

## 使用示例

```go
clk := myClock            // 实现 agentpromo.Clock 接口；传 nil 用真实时钟
s := agentpromo.New(clk)

_ = s.SetPolicy("org1", agentpromo.Policy{
    MaxTools:           3,
    RequiredPromptKeys: []string{"system"},
    ReviewWait:         time.Hour,
    MaxDeploymentSlots: 2,
})

v, _ := s.NewDraft("org1", "agent1", agentpromo.Config{
    Tools:  []string{"search"},
    Prompt: map[string]string{"system": "you are helpful"},
})

_ = s.Advance("org1", "agent1", v, agentpromo.ActionSubmit)
// …等待 ReviewWait 之后…
_ = s.Advance("org1", "agent1", v, agentpromo.ActionApprove)
_ = s.Advance("org1", "agent1", v, agentpromo.ActionDeploy)

snap := s.Read("org1") // 一致快照：版本状态、审批绑定、槽位占用
```

`Advance` 只作用于显式指定的版本，绝不自动改操作最新版本；失败操作不留下任何部分状态。

## 测试

```sh
go test ./...
```

覆盖：注入时钟的等待期边界、策略更新使未部署审批失效后重新审批、单槽位下两 agent 并发部署恰有一个成功且失败者保留状态与审批、热替换单槽原子性、槽位上限下调失败、版本号不复用、批准校验。
