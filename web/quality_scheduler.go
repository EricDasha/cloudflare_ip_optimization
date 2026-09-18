package main

import (
	"errors"
	"math"
	"sort"
	"time"
)

// ============================================================================
// 三池模型状态机 (Three-Pool Model State Machine)
// ============================================================================
//
// 状态定义：
//   UNKNOWN  → 未测试
//   PROBING  → 正在测试中
//   STANDBY  → 已通过测试，进入替补池，等待晋升机会
//   ACTIVE   → 工作池，正在转发用户流量
//   DEGRADED → 工作池但性能下降（连接失败但未达阈值）
//   FAILED   → 测试失败，暂时淘汰
//
// 状态转换：
//   UNKNOWN  → PROBING
//   PROBING  → STANDBY | FAILED
//   STANDBY  → PROBING | ACTIVE | FAILED
//   ACTIVE   → DEGRADED | FAILED | STANDBY（被更高品质 IP 替换）
//   DEGRADED → ACTIVE | FAILED | STANDBY
//   FAILED   → PROBING（重新测试）
//
// 晋升阈值（Promotion Threshold，防抖版）：
//   - RequiredSuperiorRounds = 3：连续 3 轮证明明显优于 Active
//   - RelativePromotionGain = 0.25：速度提升 ≥25%
//   - AbsolutePromotionGainMbps = 80：且绝对提升 ≥80Mbps
//   - FailureThreshold = 3：连续失败 3 次才判失败（单次抖动不再换池）
//   - MinimumSwitchInterval = 30min：距离上次切换 >30 分钟
//
// ============================================================================

type lineState string

const (
	lineUnknown  lineState = "UNKNOWN"
	lineProbing  lineState = "PROBING"
	lineStandby  lineState = "STANDBY"
	lineActive   lineState = "ACTIVE"
	lineDegraded lineState = "DEGRADED"
	lineFailed   lineState = "FAILED"
)

type switchEvent string

const (
	switchNone      switchEvent = "NONE"
	switchPromotion switchEvent = "PROMOTION"
	switchFailover  switchEvent = "FAILOVER"
)

const (
	reasonProbeStarted               = "probe_started"
	reasonProbePassed                = "probe_passed"
	reasonPromotedSuperiorThroughput = "promoted_by_superior_throughput"
	reasonPromotedNoActive           = "promoted_without_active"
	reasonThroughputDrop             = "throughput_drop"
	reasonProbeTimeout               = "probe_timeout"
	reasonFailoverAfterActiveFailure = "failover_after_active_failure"
)

// lineQuality 表示单个 IP 的质量状态，用于三池模型的晋升决策。
type lineQuality struct {
	IP                  string
	State               lineState
	StateReason         string
	AverageMbps         float64
	P95Mbps             float64
	PeakMbps            float64
	SuccessRate         float64
	ConsecutiveSuperior int
	ConsecutiveFailures int
	StateChangedAt      time.Time
}

// schedulerPolicy 定义晋升阈值和切换规则。
// 这些参数共同防止系统过度抖动（oscillation）：
//   - CapacityMbps：速度上限，防止异常高值影响评分
//   - RelativePromotionGain：相对提升阈值（25%）
//   - AbsolutePromotionGainMbps：绝对提升阈值（80Mbps）
//   - RequiredSuperiorRounds：连续证明轮数（3轮）
//   - FailureThreshold：失败次数阈值（3次）
//   - MinimumSwitchInterval：最小切换间隔（30分钟）
type schedulerPolicy struct {
	CapacityMbps              float64
	RelativePromotionGain     float64
	AbsolutePromotionGainMbps float64
	RequiredSuperiorRounds    int
	FailureThreshold          int
	MinimumSwitchInterval     time.Duration
}

func defaultSchedulerPolicy() schedulerPolicy {
	return schedulerPolicy{
		CapacityMbps:              1024,
		RelativePromotionGain:     0.25,
		AbsolutePromotionGainMbps: 80,
		RequiredSuperiorRounds:    3,
		FailureThreshold:          3,
		MinimumSwitchInterval:     30 * time.Minute,
	}
}

func (p schedulerPolicy) validate() error {
	if p.CapacityMbps <= 0 || p.RelativePromotionGain < 0 || p.AbsolutePromotionGainMbps < 0 {
		return errors.New("scheduler throughput policy must be non-negative")
	}
	if p.RequiredSuperiorRounds < 1 || p.FailureThreshold < 1 || p.MinimumSwitchInterval < 0 {
		return errors.New("scheduler counters and intervals must be positive")
	}
	return nil
}

type schedulerInput struct {
	Now        time.Time
	LastSwitch time.Time
	Active     *lineQuality
	Standby    []lineQuality
}

type schedulerDecision struct {
	Event  switchEvent
	FromIP string
	ToIP   string
	Reason string
}

// decideLineSwitch 根据三池模型和晋升阈值决定是否换池。
//
// 决策流程：
// 1. 如果没有 Active IP，从 Standby 中选最优晋升
// 2. 如果 Active 失败（连续失败 ≥ FailureThreshold=3），触发 Failover
// 3. 如果距离上次切换 < MinimumSwitchInterval（30min），不换
// 4. 检查是否有 Standby IP 满足晋升条件：
//   - 连续 ConsecutiveSuperior ≥ RequiredSuperiorRounds（3轮）
//   - 速度 AverageMbps ≥ Active * (1 + RelativePromotionGain)（25%）
//   - 速度 AverageMbps ≥ Active + AbsolutePromotionGainMbps（80Mbps）
//
// 5. 满足条件则执行 Promotion，否则保持当前 Active
func decideLineSwitch(input schedulerInput, policy schedulerPolicy) (schedulerDecision, error) {
	if err := policy.validate(); err != nil {
		return schedulerDecision{}, err
	}
	eligible := eligibleStandby(input.Standby, policy.CapacityMbps)
	if input.Active == nil {
		if len(eligible) == 0 {
			return schedulerDecision{Event: switchNone}, nil
		}
		return schedulerDecision{Event: switchPromotion, ToIP: eligible[0].IP, Reason: reasonPromotedNoActive}, nil
	}
	active := *input.Active
	if active.State == lineFailed || active.ConsecutiveFailures >= policy.FailureThreshold {
		if len(eligible) == 0 {
			return schedulerDecision{Event: switchNone, FromIP: active.IP, Reason: reasonFailoverAfterActiveFailure}, nil
		}
		return schedulerDecision{Event: switchFailover, FromIP: active.IP, ToIP: eligible[0].IP, Reason: reasonFailoverAfterActiveFailure}, nil
	}
	if !input.LastSwitch.IsZero() && input.Now.Sub(input.LastSwitch) < policy.MinimumSwitchInterval {
		return schedulerDecision{Event: switchNone, FromIP: active.IP}, nil
	}
	activeAverage := effectiveMbps(active.AverageMbps, policy.CapacityMbps)
	for _, candidate := range eligible {
		candidateAverage := effectiveMbps(candidate.AverageMbps, policy.CapacityMbps)
		if candidate.ConsecutiveSuperior < policy.RequiredSuperiorRounds {
			continue
		}
		if candidateAverage < activeAverage*(1+policy.RelativePromotionGain) {
			continue
		}
		if candidateAverage < activeAverage+policy.AbsolutePromotionGainMbps {
			continue
		}
		return schedulerDecision{Event: switchPromotion, FromIP: active.IP, ToIP: candidate.IP, Reason: reasonPromotedSuperiorThroughput}, nil
	}
	return schedulerDecision{Event: switchNone, FromIP: active.IP}, nil
}

func eligibleStandby(lines []lineQuality, capacityMbps float64) []lineQuality {
	eligible := make([]lineQuality, 0, len(lines))
	for _, line := range lines {
		if line.IP == "" || line.State != lineStandby || line.ConsecutiveFailures > 0 || line.SuccessRate <= 0 {
			continue
		}
		eligible = append(eligible, line)
	}
	sort.SliceStable(eligible, func(i, j int) bool {
		left, right := lineQualityScore(eligible[i], capacityMbps), lineQualityScore(eligible[j], capacityMbps)
		if left != right {
			return left > right
		}
		return eligible[i].IP < eligible[j].IP
	})
	return eligible
}

func lineQualityScore(line lineQuality, capacityMbps float64) float64 {
	average := effectiveMbps(line.AverageMbps, capacityMbps)
	p95 := effectiveMbps(line.P95Mbps, capacityMbps)
	peak := effectiveMbps(line.PeakMbps, capacityMbps)
	stability := math.Max(0, math.Min(1, line.SuccessRate)) * capacityMbps
	return average*0.60 + p95*0.20 + peak*0.10 + stability*0.10
}

func effectiveMbps(speed, capacity float64) float64 {
	if speed < 0 {
		return 0
	}
	return math.Min(speed, capacity)
}

func transitionLineState(line lineQuality, next lineState, reason string, at time.Time) (lineQuality, error) {
	allowed := map[lineState]map[lineState]bool{
		lineUnknown:  {lineProbing: true},
		lineProbing:  {lineStandby: true, lineFailed: true},
		lineStandby:  {lineProbing: true, lineActive: true, lineFailed: true},
		lineActive:   {lineDegraded: true, lineFailed: true, lineStandby: true},
		lineDegraded: {lineActive: true, lineFailed: true, lineStandby: true},
		lineFailed:   {lineProbing: true},
	}
	if reason == "" {
		return line, errors.New("state transition reason is required")
	}
	if !allowed[line.State][next] {
		return line, errors.New("illegal line state transition")
	}
	line.State = next
	line.StateReason = reason
	line.StateChangedAt = at
	return line, nil
}
