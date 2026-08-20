package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const proxyQualityFile = "proxy-quality.json"

type qualitySample struct {
	At      time.Time `json:"at"`
	Mbps    float64   `json:"mbps"`
	Success bool      `json:"success"`
}

type qualityRecord struct {
	IP                  string          `json:"ip"`
	Source              string          `json:"source,omitempty"`
	State               lineState       `json:"state"`
	StateReason         string          `json:"stateReason"`
	AverageMbps         float64         `json:"averageMbps"`
	P95Mbps             float64         `json:"p95Mbps"`
	PeakMbps            float64         `json:"peakMbps"`
	SuccessRate         float64         `json:"successRate"`
	ConsecutiveSuperior int             `json:"consecutiveSuperior"`
	ConsecutiveFailures int             `json:"consecutiveFailures"`
	LastProbeAt         time.Time       `json:"lastProbeAt,omitempty"`
	StateChangedAt      time.Time       `json:"stateChangedAt,omitempty"`
	Samples             []qualitySample `json:"samples,omitempty"`
}

func (r qualityRecord) line() lineQuality {
	return lineQuality{
		IP: r.IP, State: r.State, StateReason: r.StateReason, AverageMbps: r.AverageMbps,
		P95Mbps: r.P95Mbps, PeakMbps: r.PeakMbps, SuccessRate: r.SuccessRate,
		ConsecutiveSuperior: r.ConsecutiveSuperior, ConsecutiveFailures: r.ConsecutiveFailures,
		StateChangedAt: r.StateChangedAt,
	}
}

type qualityStoreData struct {
	Version      int                       `json:"version"`
	UpdatedAt    time.Time                 `json:"updatedAt"`
	LastProbeAt  time.Time                 `json:"lastProbeAt,omitempty"`
	LastSwitchAt time.Time                 `json:"lastSwitchAt,omitempty"`
	LastDecision schedulerDecision         `json:"lastDecision"`
	LastError    string                    `json:"lastError,omitempty"`
	ActiveIP     string                    `json:"activeIp,omitempty"`
	Records      map[string]*qualityRecord `json:"records"`
}

type qualitySchedulerRuntime struct {
	mu       sync.RWMutex
	path     string
	policy   schedulerPolicy
	apply    bool
	interval time.Duration
	batch    int
	data     qualityStoreData
}

func newQualitySchedulerRuntime(dataDir string) *qualitySchedulerRuntime {
	interval := time.Duration(envInt("PROXY_SCHEDULER_PROBE_INTERVAL_SECONDS", 300)) * time.Second
	if interval < time.Minute || interval > time.Hour {
		interval = 5 * time.Minute
	}
	batch := envInt("PROXY_SCHEDULER_BATCH_SIZE", 12)
	if batch < 1 || batch > 50 {
		batch = 12
	}
	r := &qualitySchedulerRuntime{
		path: filepath.Join(dataDir, proxyQualityFile), policy: defaultSchedulerPolicy(),
		apply: envBool("PROXY_SCHEDULER_APPLY", true), interval: interval, batch: batch,
		data: qualityStoreData{Version: 1, Records: make(map[string]*qualityRecord)},
	}
	_ = r.load()
	return r
}

func (r *qualitySchedulerRuntime) load() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, err := os.ReadFile(r.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var data qualityStoreData
	if err := json.Unmarshal(b, &data); err != nil {
		return err
	}
	if data.Version != 1 {
		return errors.New("unsupported proxy quality cache version")
	}
	if data.Records == nil {
		data.Records = make(map[string]*qualityRecord)
	}
	r.data = data
	return nil
}

func (r *qualitySchedulerRuntime) saveLocked() error {
	r.data.Version = 1
	r.data.UpdatedAt = time.Now()
	b, err := json.MarshalIndent(r.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, r.path)
}

func (r *qualitySchedulerRuntime) seedActive(ip string, now time.Time) {
	if ip == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.data.ActiveIP = ip
	rec := r.ensureRecordLocked(ip, "active_pool", now)
	rec.State, rec.StateReason, rec.StateChangedAt = lineActive, reasonPromotedNoActive, now
	_ = r.saveLocked()
}

func (r *qualitySchedulerRuntime) ensureRecordLocked(ip, source string, now time.Time) *qualityRecord {
	rec := r.data.Records[ip]
	if rec == nil {
		rec = &qualityRecord{IP: ip, Source: source, State: lineUnknown, StateChangedAt: now}
		r.data.Records[ip] = rec
	} else if source != "" {
		rec.Source = source
	}
	return rec
}

func (r *qualitySchedulerRuntime) observe(ip, source string, mbps float64, success bool, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec := r.ensureRecordLocked(ip, source, now)
	if rec.State == lineUnknown || rec.State == lineStandby || rec.State == lineFailed {
		rec.State, rec.StateReason, rec.StateChangedAt = lineProbing, reasonProbeStarted, now
	}
	rec.LastProbeAt = now
	rec.Samples = append(rec.Samples, qualitySample{At: now, Mbps: effectiveMbps(mbps, r.policy.CapacityMbps), Success: success})
	if len(rec.Samples) > 12 {
		rec.Samples = append([]qualitySample(nil), rec.Samples[len(rec.Samples)-12:]...)
	}
	r.recalculateLocked(rec)
	if success {
		rec.ConsecutiveFailures = 0
		if rec.IP != r.data.ActiveIP {
			rec.State, rec.StateReason, rec.StateChangedAt = lineStandby, reasonProbePassed, now
		}
	} else {
		rec.ConsecutiveFailures++
		if rec.ConsecutiveFailures >= r.policy.FailureThreshold {
			rec.State, rec.StateReason, rec.StateChangedAt = lineFailed, reasonProbeTimeout, now
		}
	}
	r.updateSuperiorLocked(rec)
	r.data.LastProbeAt = now
	_ = r.saveLocked()
}

func (r *qualitySchedulerRuntime) observeActiveHealth(success bool, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec := r.data.Records[r.data.ActiveIP]
	if rec == nil {
		return
	}
	rec.LastProbeAt = now
	if success {
		rec.ConsecutiveFailures = 0
		if rec.State == lineDegraded {
			rec.State, rec.StateReason, rec.StateChangedAt = lineActive, reasonProbePassed, now
		}
	} else {
		rec.ConsecutiveFailures++
		if rec.ConsecutiveFailures >= r.policy.FailureThreshold {
			rec.State = lineFailed
		} else {
			rec.State = lineDegraded
		}
		rec.StateReason, rec.StateChangedAt = reasonProbeTimeout, now
	}
	r.data.LastProbeAt = now
	_ = r.saveLocked()
}

func (r *qualitySchedulerRuntime) recalculateLocked(rec *qualityRecord) {
	values := make([]float64, 0, len(rec.Samples))
	successes := 0
	for _, sample := range rec.Samples {
		if sample.Success {
			values = append(values, sample.Mbps)
			successes++
		}
	}
	rec.SuccessRate = float64(successes) / float64(len(rec.Samples))
	if len(values) == 0 {
		rec.AverageMbps, rec.P95Mbps, rec.PeakMbps = 0, 0, 0
		return
	}
	sort.Float64s(values)
	var total float64
	for _, value := range values {
		total += value
	}
	rec.AverageMbps = total / float64(len(values))
	rec.PeakMbps = values[len(values)-1]
	index := int(float64(len(values)-1) * 0.95)
	rec.P95Mbps = values[index]
}

func (r *qualitySchedulerRuntime) updateSuperiorLocked(rec *qualityRecord) {
	active := r.data.Records[r.data.ActiveIP]
	if rec.IP == r.data.ActiveIP || active == nil || rec.State != lineStandby {
		rec.ConsecutiveSuperior = 0
		return
	}
	if rec.AverageMbps >= active.AverageMbps*(1+r.policy.RelativePromotionGain) && rec.AverageMbps >= active.AverageMbps+r.policy.AbsolutePromotionGainMbps {
		rec.ConsecutiveSuperior++
	} else {
		rec.ConsecutiveSuperior = 0
	}
}

func (r *qualitySchedulerRuntime) decide(now time.Time) schedulerDecision {
	r.mu.Lock()
	defer r.mu.Unlock()
	var active *lineQuality
	if rec := r.data.Records[r.data.ActiveIP]; rec != nil {
		value := rec.line()
		active = &value
	}
	standby := make([]lineQuality, 0, len(r.data.Records))
	for _, rec := range r.data.Records {
		if rec.State == lineStandby {
			standby = append(standby, rec.line())
		}
	}
	decision, err := decideLineSwitch(schedulerInput{Now: now, LastSwitch: r.data.LastSwitchAt, Active: active, Standby: standby}, r.policy)
	if err != nil {
		r.data.LastError = err.Error()
		_ = r.saveLocked()
		return schedulerDecision{Event: switchNone, Reason: err.Error()}
	}
	r.data.LastDecision = decision
	_ = r.saveLocked()
	return decision
}

func (r *qualitySchedulerRuntime) commitSwitch(decision schedulerDecision, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if old := r.data.Records[decision.FromIP]; old != nil {
		old.State, old.StateReason, old.StateChangedAt = lineStandby, decision.Reason, now
	}
	if next := r.data.Records[decision.ToIP]; next != nil {
		next.State, next.StateReason, next.StateChangedAt = lineActive, decision.Reason, now
		next.ConsecutiveSuperior = 0
	}
	r.data.ActiveIP, r.data.LastSwitchAt, r.data.LastDecision, r.data.LastError = decision.ToIP, now, decision, ""
	_ = r.saveLocked()
}

func (r *qualitySchedulerRuntime) setError(message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.data.LastError = message
	_ = r.saveLocked()
}

func (r *qualitySchedulerRuntime) snapshot() map[string]any {
	r.mu.RLock()
	defer r.mu.RUnlock()
	records := make([]qualityRecord, 0, len(r.data.Records))
	for _, rec := range r.data.Records {
		copyRec := *rec
		copyRec.Samples = nil
		records = append(records, copyRec)
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].State != records[j].State {
			return records[i].State < records[j].State
		}
		return lineQualityScore(records[i].line(), r.policy.CapacityMbps) > lineQualityScore(records[j].line(), r.policy.CapacityMbps)
	})
	return map[string]any{
		"enabled": true, "apply": r.apply, "probeIntervalSeconds": int(r.interval.Seconds()),
		"batchSize": r.batch, "activeIp": r.data.ActiveIP, "lastProbeAt": r.data.LastProbeAt,
		"lastSwitchAt": r.data.LastSwitchAt, "lastDecision": r.data.LastDecision,
		"lastError": r.data.LastError, "records": records,
	}
}

func (r *qualitySchedulerRuntime) activeIP() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.data.ActiveIP
}
