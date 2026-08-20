# Web Quality Scheduler

quality_scheduler.go 是线路质量调度器的纯逻辑第一阶段。它接收已完成探测的 Active/Standby 质量记录，只返回 NONE、PROMOTION 或 FAILOVER 决策；本阶段不执行网络请求、不启动进程、不修改 CFnat。

## 验证

    go test ./web

模拟测试覆盖正常波动、双阈值 Promotion、最短切换间隔、故障立即 Failover、容量封顶、状态原因和振荡抑制。
