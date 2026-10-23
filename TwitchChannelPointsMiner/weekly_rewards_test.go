package twitchchannelpointsminer

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"TwitchChannelPointsMiner/TwitchChannelPointsMiner/classes/entities"
)

var errFakeNoSync = errors.New("no sync in this test")

type fakeWeeklyWatcher struct {
	mu          sync.Mutex
	clipResult  func(s *entities.Streamer) bool
	vodResult   func(s *entities.Streamer) bool
	viewable    map[string]bool
	viewChecks  map[string]int
	clipCalls   int
	vodCalls    int
	blockUntil  bool // clip/VOD watches block until cancelled
	lastClip    entities.Clip
	lastVOD     entities.VOD
	lastClipMax time.Duration
	// ? fetch is the GQL state returned by FetchWeeklyRewards; nil fetch keeps the stored state.
	fetch      func(channelID string) (*entities.WeeklyRewards, error)
	fetchCalls int
}

func (f *fakeWeeklyWatcher) FetchWeeklyRewards(channelID string) (*entities.WeeklyRewards, error) {
	f.mu.Lock()
	f.fetchCalls++
	fetch := f.fetch
	f.mu.Unlock()
	if fetch != nil {
		return fetch(channelID)
	}
	return nil, errFakeNoSync
}

func (f *fakeWeeklyWatcher) SimulateClipPlayback(s *entities.Streamer, clip entities.Clip, maxWatch time.Duration, cancel <-chan struct{}, stillMissing func() bool) bool {
	f.mu.Lock()
	f.clipCalls++
	f.lastClip = clip
	f.lastClipMax = maxWatch
	block, result := f.blockUntil, f.clipResult
	f.mu.Unlock()
	if block {
		<-cancel
		return false
	}
	if result != nil {
		return result(s)
	}
	return false
}

func (f *fakeWeeklyWatcher) SimulateVODPlayback(s *entities.Streamer, vod entities.VOD, maxWatch time.Duration, cancel <-chan struct{}, stillMissing func() bool) bool {
	f.mu.Lock()
	f.vodCalls++
	f.lastVOD = vod
	block, result := f.blockUntil, f.vodResult
	f.mu.Unlock()
	if block {
		<-cancel
		return false
	}
	if result != nil {
		return result(s)
	}
	return false
}

func (f *fakeWeeklyWatcher) VODViewable(s *entities.Streamer, vodID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.viewChecks == nil {
		f.viewChecks = map[string]int{}
	}
	f.viewChecks[vodID]++
	viewable, ok := f.viewable[vodID]
	if !ok {
		return true, nil
	}
	return viewable, nil
}

func (f *fakeWeeklyWatcher) calls() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clipCalls, f.vodCalls
}

func weeklyMissingStreamer(name, channelID string) *entities.Streamer {
	s := &entities.Streamer{
		Username:  name,
		ChannelID: channelID,
		Settings:  entities.StreamerSettings{WeeklyRewards: true},
		Stream:    entities.NewStream(),
	}
	s.SetWeeklyRewards(&entities.WeeklyRewards{
		EventConfig: entities.WeeklyRewardsEventConfig{
			DaysRequiredPerWeek: 3,
			RewardTiers:         []entities.WeeklyRewardTier{{Tier: 1}, {Tier: 2}, {Tier: 3}, {Tier: 4}},
		},
	})
	s.SetWeeklyRewardsMedia(
		[]entities.Clip{{ID: name + "-clip", DurationSeconds: 30}},
		[]entities.VOD{{ID: name + "-vod", LengthSeconds: 3600}},
	)
	return s
}

// ? markVisited simulates the PubSub notification that follows a successful watch.
func markVisited(s *entities.Streamer) bool {
	_, ok := s.ApplyWeeklyRewardsNotification(entities.WeeklyRewardsNotification{NotificationType: "DAY_COMPLETED", DaysVisitedThisWeek: 1})
	return ok
}

func newTestProgressor(watcher weeklyRewardsWatcher, settings WeeklyRewardsProgressorSettings) (*weeklyRewardsProgressor, *time.Time) {
	m := &Miner{logger: discardLogger()}
	p := newWeeklyRewardsProgressor(m, watcher, settings)
	clock := time.Date(2026, time.October, 23, 12, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return clock }
	return p, &clock
}

func TestWeeklyRewardsProgressorSettingsNormalized(t *testing.T) {
	got := WeeklyRewardsProgressorSettings{Enabled: false, MaxConcurrentWatch: 0, MaxSecondsClips: -1, FailureCooldownSeconds: -5}.Normalized()
	def := DefaultWeeklyRewardsProgressorSettings()
	if got.Enabled {
		t.Fatalf("Enabled must not be forced on")
	}
	if got.MaxConcurrentWatch != def.MaxConcurrentWatch || got.MaxSecondsClips != def.MaxSecondsClips || got.MaxSecondsVODs != def.MaxSecondsVODs ||
		got.LoopIntervalSeconds != def.LoopIntervalSeconds || got.MaxFailuresPerStreamer != def.MaxFailuresPerStreamer || got.FailureCooldownSeconds != def.FailureCooldownSeconds {
		t.Fatalf("invalid values should fall back to defaults, got %+v", got)
	}
	zeroCooldown := WeeklyRewardsProgressorSettings{FailureCooldownSeconds: 0}.Normalized()
	if zeroCooldown.FailureCooldownSeconds != 0 {
		t.Fatalf("a zero cooldown is valid and must be kept")
	}
}

func TestWeeklyRewardsProgressorSelectStreamers(t *testing.T) {
	watcher := &fakeWeeklyWatcher{viewable: map[string]bool{"subonly-vod": false}}
	p, _ := newTestProgressor(watcher, DefaultWeeklyRewardsProgressorSettings())

	eligible := weeklyMissingStreamer("eligible", "1")
	online := weeklyMissingStreamer("online", "2")
	online.IsOnline = true
	visited := weeklyMissingStreamer("visited", "3")
	markVisited(visited)
	cooling := weeklyMissingStreamer("cooling", "4")
	p.cooldowns["4"] = p.now()
	watching := weeklyMissingStreamer("watching", "5")
	noMedia := weeklyMissingStreamer("nomedia", "6")
	noMedia.SetWeeklyRewardsMedia(
		[]entities.Clip{{ID: "short-clip", DurationSeconds: 5}},
		[]entities.VOD{{ID: "short-vod", LengthSeconds: 300}, {ID: "subonly-vod", LengthSeconds: 7200}},
	)
	disabled := weeklyMissingStreamer("disabled", "7")
	disabled.Settings.WeeklyRewards = false
	second := weeklyMissingStreamer("second", "8")

	all := []*entities.Streamer{online, visited, cooling, watching, noMedia, disabled, eligible, second}
	got := p.selectStreamers(all, map[string]struct{}{"5": {}}, 2)
	if len(got) != 2 || got[0] != eligible || got[1] != second {
		names := []string{}
		for _, s := range got {
			names = append(names, s.Username)
		}
		t.Fatalf("expected eligible and second, got %v", names)
	}
	if got := p.selectStreamers(all, nil, 1); len(got) != 1 || got[0] != watching {
		t.Fatalf("limit should be respected and order kept")
	}
	if got := p.selectStreamers(all, nil, 0); got != nil {
		t.Fatalf("no free slots should select nobody")
	}

	// ? Viewability is checked once per VOD and the answer is cached.
	p.selectStreamers(all, nil, 10)
	if watcher.viewChecks["subonly-vod"] != 1 {
		t.Fatalf("VOD viewability should be cached, checked %d times", watcher.viewChecks["subonly-vod"])
	}
	if _, checked := watcher.viewChecks["short-vod"]; checked {
		t.Fatalf("VODs shorter than the minimum should not be checked")
	}
}

func TestWeeklyRewardsProgressorDoWatch(t *testing.T) {
	settings := DefaultWeeklyRewardsProgressorSettings()

	t.Run("clip succeeds", func(t *testing.T) {
		watcher := &fakeWeeklyWatcher{clipResult: markVisited}
		p, _ := newTestProgressor(watcher, settings)
		s := weeklyMissingStreamer("a", "1")
		res := p.doWatch(s, make(chan struct{}))
		if !res.success || res.reason != "clip" {
			t.Fatalf("unexpected result %+v", res)
		}
		if clips, vods := watcher.calls(); clips != 1 || vods != 0 {
			t.Fatalf("VOD should not be watched after a successful clip (%d/%d)", clips, vods)
		}
		if watcher.lastClipMax != 30*time.Second {
			t.Fatalf("clip max watch should come from the settings, got %s", watcher.lastClipMax)
		}
	})

	t.Run("falls back to VOD", func(t *testing.T) {
		watcher := &fakeWeeklyWatcher{vodResult: markVisited}
		p, _ := newTestProgressor(watcher, settings)
		res := p.doWatch(weeklyMissingStreamer("b", "2"), make(chan struct{}))
		if !res.success || res.reason != "vod" || watcher.lastVOD.ID != "b-vod" {
			t.Fatalf("unexpected result %+v (vod %q)", res, watcher.lastVOD.ID)
		}
	})

	t.Run("failure reasons", func(t *testing.T) {
		watcher := &fakeWeeklyWatcher{}
		p, _ := newTestProgressor(watcher, settings)
		s := weeklyMissingStreamer("c", "3")
		if res := p.doWatch(s, make(chan struct{})); res.success || res.reason != "clip and VOD both timed out" {
			t.Fatalf("unexpected result %+v", res)
		}
		s.SetWeeklyRewardsMedia(nil, []entities.VOD{{ID: "v", LengthSeconds: 3600}})
		if res := p.doWatch(s, make(chan struct{})); res.reason != "VOD timed out" {
			t.Fatalf("unexpected result %+v", res)
		}
		s.SetWeeklyRewardsMedia([]entities.Clip{{ID: "c", DurationSeconds: 30}}, nil)
		if res := p.doWatch(s, make(chan struct{})); res.reason != "clip timed out and the channel has no viewable VODs" {
			t.Fatalf("unexpected result %+v", res)
		}
		s.SetWeeklyRewardsMedia(nil, nil)
		if res := p.doWatch(s, make(chan struct{})); res.reason != "channel has no usable clips or VODs" {
			t.Fatalf("unexpected result %+v", res)
		}
	})

	t.Run("cancelled before the VOD", func(t *testing.T) {
		cancel := make(chan struct{})
		watcher := &fakeWeeklyWatcher{clipResult: func(*entities.Streamer) bool { close(cancel); return false }}
		p, _ := newTestProgressor(watcher, settings)
		res := p.doWatch(weeklyMissingStreamer("d", "4"), cancel)
		if res.success || res.reason != "cancelled" {
			t.Fatalf("unexpected result %+v", res)
		}
		if _, vods := watcher.calls(); vods != 0 {
			t.Fatalf("a cancelled watch must not continue with the VOD")
		}
	})

	t.Run("missed PubSub message is confirmed by a sync", func(t *testing.T) {
		watcher := &fakeWeeklyWatcher{}
		s := weeklyMissingStreamer("f", "6")
		watcher.fetch = func(channelID string) (*entities.WeeklyRewards, error) {
			if channelID != "6" {
				t.Errorf("unexpected channel %s", channelID)
			}
			snap, _ := s.WeeklyRewardsSnapshot()
			snap.HasVisitedToday = true
			return &snap, nil
		}
		p, _ := newTestProgressor(watcher, settings)
		res := p.doWatch(s, make(chan struct{}))
		if !res.success || res.reason != "clip (confirmed by sync)" {
			t.Fatalf("unexpected result %+v", res)
		}
		if _, vods := watcher.calls(); vods != 0 {
			t.Fatalf("a confirmed clip must not continue with the VOD")
		}
		if s.MissingWeeklyReward(time.Now()) {
			t.Fatalf("the synced state should be stored on the streamer")
		}
	})

	t.Run("sync after a failed VOD", func(t *testing.T) {
		watcher := &fakeWeeklyWatcher{}
		s := weeklyMissingStreamer("g", "7")
		s.SetWeeklyRewardsMedia(nil, []entities.VOD{{ID: "v", LengthSeconds: 3600}})
		watcher.fetch = func(string) (*entities.WeeklyRewards, error) {
			snap, _ := s.WeeklyRewardsSnapshot()
			snap.HasVisitedToday = true
			return &snap, nil
		}
		p, _ := newTestProgressor(watcher, settings)
		if res := p.doWatch(s, make(chan struct{})); !res.success || res.reason != "vod (confirmed by sync)" {
			t.Fatalf("unexpected result %+v", res)
		}
	})

	t.Run("panic is reported as failure", func(t *testing.T) {
		watcher := &fakeWeeklyWatcher{clipResult: func(*entities.Streamer) bool { panic("boom") }}
		p, _ := newTestProgressor(watcher, settings)
		if res := p.doWatch(weeklyMissingStreamer("e", "5"), make(chan struct{})); res.success || !strings.Contains(res.reason, "boom") {
			t.Fatalf("unexpected result %+v", res)
		}
	})
}

func waitForSlotResult(t *testing.T, p *weeklyRewardsProgressor, slots []*weeklyRewardsSlot) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		busy := false
		for _, s := range slots {
			if s != nil && len(s.done) == 0 {
				busy = true
			}
		}
		if !busy {
			p.manageSlots(slots)
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("watch did not finish in time")
}

func TestWeeklyRewardsProgressorFailureCooldown(t *testing.T) {
	settings := DefaultWeeklyRewardsProgressorSettings()
	settings.MaxConcurrentWatch = 1
	settings.MaxFailuresPerStreamer = 2
	settings.FailureCooldownSeconds = 600
	watcher := &fakeWeeklyWatcher{}
	p, clock := newTestProgressor(watcher, settings)
	s := weeklyMissingStreamer("fails", "1")
	streamers := []*entities.Streamer{s}
	slots := make([]*weeklyRewardsSlot, 1)

	// ? First failure: counted, not yet on cooldown.
	p.fillSlots(slots, streamers)
	waitForSlotResult(t, p, slots)
	if p.failures["1"] != 1 || len(p.cooldowns) != 0 {
		t.Fatalf("after one failure: failures=%v cooldowns=%v", p.failures, p.cooldowns)
	}
	// ? Second failure reaches the limit and starts the cooldown.
	p.fillSlots(slots, streamers)
	waitForSlotResult(t, p, slots)
	if _, cooling := p.cooldowns["1"]; !cooling || p.failures["1"] != 0 {
		t.Fatalf("after two failures: failures=%v cooldowns=%v", p.failures, p.cooldowns)
	}
	p.fillSlots(slots, streamers)
	if slots[0] != nil {
		t.Fatalf("a streamer on cooldown must not be watched")
	}

	*clock = clock.Add(11 * time.Minute)
	p.manageCooldowns()
	p.fillSlots(slots, streamers)
	if slots[0] == nil {
		t.Fatalf("the streamer should be retried after the cooldown")
	}
	waitForSlotResult(t, p, slots)

	// ? A success clears the failure state.
	watcher.mu.Lock()
	watcher.clipResult = markVisited
	watcher.mu.Unlock()
	*clock = clock.Add(11 * time.Minute)
	p.manageCooldowns()
	p.failures["1"] = 1
	p.fillSlots(slots, streamers)
	waitForSlotResult(t, p, slots)
	if len(p.failures) != 0 || len(p.cooldowns) != 0 {
		t.Fatalf("success should clear failures/cooldowns: %v %v", p.failures, p.cooldowns)
	}
	if s.MissingWeeklyReward(*clock) {
		t.Fatalf("streamer should no longer be missing today's visit")
	}
	p.fillSlots(slots, streamers)
	if slots[0] != nil {
		t.Fatalf("a streamer that made progress today must not be watched again")
	}
}

func TestWeeklyRewardsProgressorTimeoutCancelsWatch(t *testing.T) {
	settings := DefaultWeeklyRewardsProgressorSettings()
	settings.MaxConcurrentWatch = 2
	watcher := &fakeWeeklyWatcher{blockUntil: true}
	p, clock := newTestProgressor(watcher, settings)
	a, b := weeklyMissingStreamer("a", "1"), weeklyMissingStreamer("b", "2")
	slots := make([]*weeklyRewardsSlot, 2)

	p.fillSlots(slots, []*entities.Streamer{a, b})
	if slots[0] == nil || slots[1] == nil || slots[0].streamer == slots[1].streamer {
		t.Fatalf("both slots should be filled with different streamers")
	}
	cancelA, cancelB := slots[0].cancel, slots[1].cancel

	p.manageSlots(slots)
	if slots[0] == nil || slots[1] == nil {
		t.Fatalf("running watches must be kept before the timeout")
	}

	*clock = clock.Add(p.fullTimeout() + time.Second)
	p.manageSlots(slots)
	if slots[0] != nil || slots[1] != nil {
		t.Fatalf("timed out slots should be freed")
	}
	for _, c := range []chan struct{}{cancelA, cancelB} {
		select {
		case <-c:
		default:
			t.Fatalf("timed out watches should be cancelled")
		}
	}
	if len(p.cooldowns) != 2 {
		t.Fatalf("timeouts count as failures (limit 1 => cooldown), got %v", p.cooldowns)
	}
}

func TestWeeklyRewardsProgressorRunStops(t *testing.T) {
	settings := DefaultWeeklyRewardsProgressorSettings()
	settings.LoopIntervalSeconds = 0.005
	watcher := &fakeWeeklyWatcher{blockUntil: true}
	p, _ := newTestProgressor(watcher, settings)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		p.run([]*entities.Streamer{weeklyMissingStreamer("a", "1")}, stop)
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if clips, _ := watcher.calls(); clips > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("progressor never started watching")
		}
		time.Sleep(time.Millisecond)
	}
	close(stop)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("progressor did not stop")
	}
}

func TestHandleWeeklyRewardNotification(t *testing.T) {
	m := &Miner{logger: discardLogger()}
	s := weeklyMissingStreamer("notified", "1")
	m.handleWeeklyRewardNotification(s, entities.WeeklyRewardsNotification{NotificationType: "DAY_COMPLETED", DaysVisitedThisWeek: 2})
	snap, _ := s.WeeklyRewardsSnapshot()
	if !snap.HasVisitedToday || snap.DaysVisitedThisWeek != 2 {
		t.Fatalf("notification should update the state: %+v", snap)
	}
	// ? Unsynced streamers and nil are ignored without panicking.
	m.handleWeeklyRewardNotification(&entities.Streamer{Username: "x"}, entities.WeeklyRewardsNotification{NotificationType: "DAY_COMPLETED"})
	m.handleWeeklyRewardNotification(nil, entities.WeeklyRewardsNotification{})
}

func TestPickStreamersToWatchWeeklyRewardsPriority(t *testing.T) {
	onlineAt := time.Now().Add(-time.Minute)
	plain := &entities.Streamer{Username: "plain", IsOnline: true, OnlineAt: onlineAt, Stream: entities.NewStream()}
	plain2 := &entities.Streamer{Username: "plain2", IsOnline: true, OnlineAt: onlineAt, Stream: entities.NewStream()}
	weekly := weeklyMissingStreamer("weekly", "3")
	weekly.IsOnline = true
	weekly.OnlineAt = onlineAt
	weekly.Stream.WatchStreakMissing = true
	visited := weeklyMissingStreamer("visited", "4")
	visited.IsOnline = true
	visited.OnlineAt = onlineAt
	visited.Stream.WatchStreakMissing = true
	markVisited(visited)

	m := &Miner{watchPriorities: parseWatchPriorities([]string{"WEEKLY_REWARDS", "ORDER"})}
	got := m.pickStreamersToWatch([]*entities.Streamer{plain, visited, plain2, weekly})
	if len(got) != 2 || got[0] != weekly || got[1] != plain {
		names := []string{}
		for _, s := range got {
			names = append(names, s.Username)
		}
		t.Fatalf("expected weekly first then plain, got %v", names)
	}

	if p := parseWatchPriorities([]string{"weekly"}); len(p) != 1 || p[0] != watchPriorityWeeklyRewards {
		t.Fatalf("WEEKLY alias should parse, got %v", p)
	}
}

func TestWeeklyRewardsEnabled(t *testing.T) {
	off := &entities.Streamer{}
	on := &entities.Streamer{Settings: entities.StreamerSettings{WeeklyRewards: true}}
	if weeklyRewardsEnabled([]*entities.Streamer{off, nil}) || !weeklyRewardsEnabled([]*entities.Streamer{off, on}) {
		t.Fatalf("weeklyRewardsEnabled should be true only with an enabled streamer")
	}
	m := &Miner{}
	if m.weeklyRewardsProgressorSettings() != DefaultWeeklyRewardsProgressorSettings() {
		t.Fatalf("unset progressor settings should use the defaults")
	}
	m.SetWeeklyRewardsProgressorSettings(WeeklyRewardsProgressorSettings{Enabled: false, MaxConcurrentWatch: 4})
	if got := m.weeklyRewardsProgressorSettings(); got.Enabled || got.MaxConcurrentWatch != 4 || got.MaxSecondsVODs != 480 {
		t.Fatalf("settings should be stored normalized, got %+v", got)
	}
}
