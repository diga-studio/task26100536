package agentpromo

import "time"

// Clock 可注入时钟；假定时间不倒退。
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }
