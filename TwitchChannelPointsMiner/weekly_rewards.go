package twitchchannelpointsminer

import (
	"fmt"
	"sync"
	"time"

	"TwitchChannelPointsMiner/TwitchChannelPointsMiner/classes/entities"
	"TwitchChannelPointsMiner/TwitchChannelPointsMiner/constants"
)

// ? Weekly Rewards: state sync, PubSub updates and progression through Clips/VODs for offline
// ? channels. Ported from mpforce1/Twitch-Channel-Points-Miner PR #29.

const (
	weeklyRewardsSyncInterval  = time.Hour
	weeklyRewardsMediaPause    = time.Second
	weeklyRewardsMinClipLength = 5      // seconds; shorter clips are skipped
	weeklyRewardsMinVODLength  = 6 * 60 // seconds; shorter VODs are skipped
	weeklyRewardsTimeoutMargin = 10 * time.Second
)

// WeeklyRewardsProgressorSettings controls how the miner makes Weekly Rewards progress for
// ? offline channels by "watching" Clips and VODs.
type WeeklyRewardsProgressorSettings struct {
	Enabled                bool
	MaxConcurrentWatch     int
	MaxSecondsClips        float64
	MaxSecondsVODs         float64
	LoopIntervalSeconds    float64
	MaxFailuresPerStreamer int
	FailureCooldownSeconds float64
}

// DefaultWeeklyRewardsProgressorSettings mirrors the defaults of the Python miner.
func DefaultWeeklyRewardsProgressorSettings() WeeklyRewardsProgressorSettings {
	return WeeklyRewardsProgressorSettings{
		Enabled:                true,
		MaxConcurrentWatch:     2,
		MaxSecondsClips:        30,
		MaxSecondsVODs:         8 * 60,
		LoopIntervalSeconds:    20,
		MaxFailuresPerStreamer: 1,
		FailureCooldownSeconds: 60 * 60,
	}
}

// Normalized replaces invalid values with the defaults.
func (s WeeklyRewardsProgressorSettings) Normalized() WeeklyRewardsProgressorSettings {
	def := DefaultWeeklyRewardsProgressorSettings()
	if s.MaxConcurrentWatch <= 0 {
		s.MaxConcurrentWatch = def.MaxConcurrentWatch
	}
	if s.MaxSecondsClips <= 0 {
		s.MaxSecondsClips = def.MaxSecondsClips
	}
	if s.MaxSecondsVODs <= 0 {
		s.MaxSecondsVODs = def.MaxSecondsVODs
	}
	if s.LoopIntervalSeconds <= 0 {
		s.LoopIntervalSeconds = def.LoopIntervalSeconds
	}
	if s.MaxFailuresPerStreamer <= 0 {
		s.MaxFailuresPerStreamer = def.MaxFailuresPerStreamer
	}
	if s.FailureCooldownSeconds < 0 {
		s.FailureCooldownSeconds = def.FailureCooldownSeconds
	}
	return s
}

func secondsDuration(seconds float64) time.Duration {
	return time.Duration(seconds * float64(time.Second))
}

// SetWeeklyRewardsProgressorSettings configures Weekly Rewards progression (call before Mine).
func (m *Miner) SetWeeklyRewardsProgressorSettings(settings WeeklyRewardsProgressorSettings) {
	normalized := settings.Normalized()
	m.weeklyProgressorSettings = &normalized
}

func (m *Miner) weeklyRewardsProgressorSettings() WeeklyRewardsProgressorSettings {
	if m.weeklyProgressorSettings == nil {
		return DefaultWeeklyRewardsProgressorSettings()
	}
	return *m.weeklyProgressorSettings
}

func weeklyRewardsEnabled(streamers []*entities.Streamer) bool {
	for _, s := range streamers {
		if s != nil && s.Settings.WeeklyRewards {
			return true
		}
	}
	return false
}

// ? startWeeklyRewards launches the syncer and, when enabled, the progressor.
func (m *Miner) startWeeklyRewards(streamers []*entities.Streamer, stop <-chan struct{}) {
	if !weeklyRewardsEnabled(streamers) || m.twitch == nil {
		return
	}
	go m.weeklyRewardsSyncer(streamers, stop)
	settings := m.weeklyRewardsProgressorSettings()
	if settings.Enabled {
		progressor := newWeeklyRewardsProgressor(m, m.twitch, settings)
		go progressor.run(streamers, stop)
	}
}

func (m *Miner) weeklyRewardsSyncer(streamers []*entities.Streamer, stop <-chan struct{}) {
	ticker := time.NewTicker(weeklyRewardsSyncInterval)
	defer ticker.Stop()
	first := true
	for {
		if m.syncWeeklyRewards(streamers, stop, first) {
			return
		}
		first = false
		select {
		case <-ticker.C:
		case <-stop:
			return
		}
	}
}

// ? syncWeeklyRewards refreshes the reward state (and Clips/VODs for channels with an event).
// ? It returns true when stop was closed.
func (m *Miner) syncWeeklyRewards(streamers []*entities.Streamer, stop <-chan struct{}, first bool) bool {
	active, missing := 0, 0
	now := time.Now()
	for _, s := range streamers {
		if s == nil || !s.Settings.WeeklyRewards || s.ChannelID == "" {
			continue
		}
		select {
		case <-stop:
			return true
		default:
		}
		rewards, err := m.twitch.FetchWeeklyRewards(s.ChannelID)
		if err != nil {
			m.logger.Errorf("weekly rewards sync %s: %v", m.styledStreamerName(s), err)
			continue
		}
		s.SetWeeklyRewards(rewards)
		if rewards == nil {
			s.SetWeeklyRewardsMedia(nil, nil)
			continue
		}
		active++
		if !s.MissingWeeklyReward(now) {
			continue
		}
		missing++
		// ? Clips/VODs are only needed while today's visit is still missing.
		clips, vods, err := m.twitch.FetchWeeklyRewardsMedia(s)
		if err != nil {
			m.logger.Debugf("weekly rewards media %s: %v", m.rawStreamerName(s.Username), err)
		}
		if len(clips) > 0 || len(vods) > 0 || err == nil {
			s.SetWeeklyRewardsMedia(clips, vods)
		}
		if m.sleepWithStop(weeklyRewardsMediaPause, stop) {
			return true
		}
	}
	if active > 0 || first {
		m.logger.EmojiPrintf(":calendar:", "Weekly Rewards: %d channel(s) with an active event, %d still to visit today", active, missing)
	}
	return false
}

var weeklyRewardsTierEmojis = []string{":seedling:", ":potted_plant:", ":wilted_flower:", ":rose:", ":bouquet:"}

func (m *Miner) handleWeeklyRewardNotification(streamer *entities.Streamer, n entities.WeeklyRewardsNotification) {
	if streamer == nil {
		return
	}
	updated, ok := streamer.ApplyWeeklyRewardsNotification(n)
	if !ok {
		m.logger.Debugf("weekly rewards notification for %s before its state was synced", m.rawStreamerName(streamer.Username))
		return
	}
	emoji := ":calendar:"
	if updated.AccumulatedWeeks >= 0 && updated.AccumulatedWeeks < len(weeklyRewardsTierEmojis) {
		emoji = weeklyRewardsTierEmojis[updated.AccumulatedWeeks]
	}
	m.logger.EmojiEventf(emoji, constants.EventWeeklyReward, "Weekly Reward %s: %s, %d/%d days visited this week",
		m.styledStreamerName(streamer), n.NotificationType, updated.DaysVisitedThisWeek, updated.EventConfig.DaysRequiredPerWeek)
}

// ? weeklyRewardsWatcher is the part of classes.Twitch the progressor uses (an interface for tests).
type weeklyRewardsWatcher interface {
	SimulateClipPlayback(streamer *entities.Streamer, clip entities.Clip, maxWatch time.Duration, cancel <-chan struct{}, stillMissing func() bool) bool
	SimulateVODPlayback(streamer *entities.Streamer, vod entities.VOD, maxWatch time.Duration, cancel <-chan struct{}, stillMissing func() bool) bool
	VODViewable(streamer *entities.Streamer, vodID string) (bool, error)
	FetchWeeklyRewards(channelID string) (*entities.WeeklyRewards, error)
}

type weeklyRewardsResult struct {
	success bool
	reason  string
}

type weeklyRewardsSlot struct {
	streamer *entities.Streamer
	started  time.Time
	done     chan weeklyRewardsResult
	cancel   chan struct{}
}

type weeklyRewardsProgressor struct {
	m           *Miner
	watcher     weeklyRewardsWatcher
	settings    WeeklyRewardsProgressorSettings
	now         func() time.Time
	failures    map[string]int
	cooldowns   map[string]time.Time
	vodMu       sync.Mutex
	vodViewable map[string]bool
}

func newWeeklyRewardsProgressor(m *Miner, watcher weeklyRewardsWatcher, settings WeeklyRewardsProgressorSettings) *weeklyRewardsProgressor {
	return &weeklyRewardsProgressor{
		m:           m,
		watcher:     watcher,
		settings:    settings.Normalized(),
		now:         time.Now,
		failures:    make(map[string]int),
		cooldowns:   make(map[string]time.Time),
		vodViewable: make(map[string]bool),
	}
}

func (p *weeklyRewardsProgressor) fullTimeout() time.Duration {
	return secondsDuration(p.settings.MaxSecondsClips) + secondsDuration(p.settings.MaxSecondsVODs) + weeklyRewardsTimeoutMargin
}

func (p *weeklyRewardsProgressor) run(streamers []*entities.Streamer, stop <-chan struct{}) {
	slots := make([]*weeklyRewardsSlot, p.settings.MaxConcurrentWatch)
	interval := secondsDuration(p.settings.LoopIntervalSeconds)
	for {
		p.manageSlots(slots)
		p.manageCooldowns()
		p.fillSlots(slots, streamers)
		timer := time.NewTimer(interval)
		select {
		case <-stop:
			timer.Stop()
			for _, slot := range slots {
				if slot != nil {
					close(slot.cancel)
				}
			}
			return
		case <-timer.C:
		}
	}
}

// ? pickClip returns the first top Clip longer than the minimum length.
func (p *weeklyRewardsProgressor) pickClip(streamer *entities.Streamer) *entities.Clip {
	clips, _ := streamer.WeeklyRewardsMedia()
	for i := range clips {
		if clips[i].DurationSeconds > weeklyRewardsMinClipLength {
			return &clips[i]
		}
	}
	return nil
}

// ? pickVOD returns the most recent viewable VOD of at least the minimum length.
func (p *weeklyRewardsProgressor) pickVOD(streamer *entities.Streamer) *entities.VOD {
	_, vods := streamer.WeeklyRewardsMedia()
	for i := range vods {
		vod := vods[i]
		if vod.LengthSeconds < weeklyRewardsMinVODLength {
			continue
		}
		if p.vodIsViewable(streamer, vod.ID) {
			return &vod
		}
	}
	return nil
}

// ? vodIsViewable caches the result per VOD so each one is checked only once.
func (p *weeklyRewardsProgressor) vodIsViewable(streamer *entities.Streamer, vodID string) bool {
	p.vodMu.Lock()
	viewable, known := p.vodViewable[vodID]
	p.vodMu.Unlock()
	if known {
		return viewable
	}
	viewable, err := p.watcher.VODViewable(streamer, vodID)
	if err != nil {
		// ? Transient errors are not cached, the VOD is checked again next time.
		p.m.logger.Debugf("weekly rewards VOD check %s: %v", p.m.rawStreamerName(streamer.Username), err)
		return false
	}
	p.vodMu.Lock()
	p.vodViewable[vodID] = viewable
	p.vodMu.Unlock()
	return viewable
}

func (p *weeklyRewardsProgressor) selectStreamers(streamers []*entities.Streamer, watching map[string]struct{}, limit int) []*entities.Streamer {
	if limit <= 0 {
		return nil
	}
	now := p.now()
	selected := make([]*entities.Streamer, 0, limit)
	for _, s := range streamers {
		if len(selected) >= limit {
			break
		}
		// ? Online channels make progress through the regular live watching.
		if s == nil || s.IsOnline || s.ChannelID == "" {
			continue
		}
		if _, ok := watching[s.ChannelID]; ok {
			continue
		}
		if _, ok := p.cooldowns[s.ChannelID]; ok {
			continue
		}
		if !s.MissingWeeklyReward(now) {
			continue
		}
		if p.pickClip(s) == nil && p.pickVOD(s) == nil {
			continue
		}
		selected = append(selected, s)
	}
	return selected
}

func (p *weeklyRewardsProgressor) fillSlots(slots []*weeklyRewardsSlot, streamers []*entities.Streamer) {
	watching := make(map[string]struct{})
	free := 0
	for _, slot := range slots {
		if slot != nil {
			watching[slot.streamer.ChannelID] = struct{}{}
		} else {
			free++
		}
	}
	targets := p.selectStreamers(streamers, watching, free)
	for _, s := range targets {
		for i := range slots {
			if slots[i] != nil {
				continue
			}
			slot := &weeklyRewardsSlot{
				streamer: s,
				started:  p.now(),
				done:     make(chan weeklyRewardsResult, 1),
				cancel:   make(chan struct{}),
			}
			slots[i] = slot
			go func() {
				slot.done <- p.doWatch(slot.streamer, slot.cancel)
			}()
			break
		}
	}
}

func (p *weeklyRewardsProgressor) doWatch(streamer *entities.Streamer, cancel <-chan struct{}) (result weeklyRewardsResult) {
	defer func() {
		if r := recover(); r != nil {
			result = weeklyRewardsResult{success: false, reason: fmt.Sprintf("panic: %v", r)}
		}
	}()
	stillMissing := func() bool { return streamer.MissingWeeklyReward(p.now()) }
	cancelled := func() bool {
		select {
		case <-cancel:
			return true
		default:
			return false
		}
	}
	clip := p.pickClip(streamer)
	if clip != nil {
		if p.watcher.SimulateClipPlayback(streamer, *clip, secondsDuration(p.settings.MaxSecondsClips), cancel, stillMissing) {
			return weeklyRewardsResult{success: true, reason: "clip"}
		}
		if cancelled() {
			return weeklyRewardsResult{success: false, reason: "cancelled"}
		}
		if p.confirmedBySync(streamer) {
			return weeklyRewardsResult{success: true, reason: "clip (confirmed by sync)"}
		}
	}
	if cancelled() {
		return weeklyRewardsResult{success: false, reason: "cancelled"}
	}
	vod := p.pickVOD(streamer)
	if vod != nil {
		if p.watcher.SimulateVODPlayback(streamer, *vod, secondsDuration(p.settings.MaxSecondsVODs), cancel, stillMissing) {
			return weeklyRewardsResult{success: true, reason: "vod"}
		}
		if cancelled() {
			return weeklyRewardsResult{success: false, reason: "cancelled"}
		}
		if p.confirmedBySync(streamer) {
			return weeklyRewardsResult{success: true, reason: "vod (confirmed by sync)"}
		}
	}
	if cancelled() {
		return weeklyRewardsResult{success: false, reason: "cancelled"}
	}
	switch {
	case clip == nil && vod == nil:
		return weeklyRewardsResult{success: false, reason: "channel has no usable clips or VODs"}
	case clip == nil:
		return weeklyRewardsResult{success: false, reason: "VOD timed out"}
	case vod == nil:
		return weeklyRewardsResult{success: false, reason: "clip timed out and the channel has no viewable VODs"}
	default:
		return weeklyRewardsResult{success: false, reason: "clip and VOD both timed out"}
	}
}

// ? confirmedBySync re-reads the state over GQL: progress normally arrives over PubSub, but if
// ? that message is missed the watch would otherwise count as a failure.
func (p *weeklyRewardsProgressor) confirmedBySync(streamer *entities.Streamer) bool {
	rewards, err := p.watcher.FetchWeeklyRewards(streamer.ChannelID)
	if err != nil {
		p.m.logger.Debugf("weekly rewards re-check %s: %v", p.m.rawStreamerName(streamer.Username), err)
		return false
	}
	streamer.SetWeeklyRewards(rewards)
	return !streamer.MissingWeeklyReward(p.now())
}

func (p *weeklyRewardsProgressor) recordFailure(channelID string) {
	failures := p.failures[channelID] + 1
	if failures >= p.settings.MaxFailuresPerStreamer {
		p.cooldowns[channelID] = p.now()
		delete(p.failures, channelID)
		return
	}
	p.failures[channelID] = failures
}

func (p *weeklyRewardsProgressor) processResult(streamer *entities.Streamer, result weeklyRewardsResult) {
	if result.success {
		p.m.logger.Debugf("weekly reward progress for %s via %s", p.m.rawStreamerName(streamer.Username), result.reason)
		delete(p.failures, streamer.ChannelID)
		delete(p.cooldowns, streamer.ChannelID)
		return
	}
	if result.reason == "cancelled" {
		return
	}
	p.recordFailure(streamer.ChannelID)
	p.m.logger.Errorf("Weekly Rewards %s: no progress with clips or VODs: %s", p.m.styledStreamerName(streamer), result.reason)
}

func (p *weeklyRewardsProgressor) manageSlots(slots []*weeklyRewardsSlot) {
	timeout := p.fullTimeout()
	for i, slot := range slots {
		if slot == nil {
			continue
		}
		select {
		case result := <-slot.done:
			slots[i] = nil
			p.processResult(slot.streamer, result)
		default:
			if p.now().Sub(slot.started) > timeout {
				slots[i] = nil
				close(slot.cancel)
				p.recordFailure(slot.streamer.ChannelID)
				p.m.logger.Errorf("Weekly Rewards %s: gave up after %s", p.m.styledStreamerName(slot.streamer), timeout)
			}
		}
	}
}

func (p *weeklyRewardsProgressor) manageCooldowns() {
	cooldown := secondsDuration(p.settings.FailureCooldownSeconds)
	now := p.now()
	for id, since := range p.cooldowns {
		if now.Sub(since) > cooldown {
			delete(p.cooldowns, id)
		}
	}
}
