package entities

import "time"

// ? Weekly Rewards: Twitch grants channel points (and a chat badge) for visiting a channel on a
// ? required number of days per week. A "visit" can be made by watching live, a Clip, or a VOD.
// ? https://help.twitch.tv/s/article/weekly-rewards

// WeeklyRewardBadge is the chat badge attached to a reward tier.
type WeeklyRewardBadge struct {
	SetID   string
	Version string
	Title   string
}

// WeeklyRewardTier is a single reward tier of a Weekly Rewards event.
type WeeklyRewardTier struct {
	Tier          int
	ChannelPoints int
	Badge         WeeklyRewardBadge
}

// WeeklyRewardsEventConfig describes the Weekly Rewards event currently running on a channel.
type WeeklyRewardsEventConfig struct {
	ID                  string
	DaysRequiredPerWeek int
	EndDate             time.Time
	WeekResetDates      []time.Time
	RewardTiers         []WeeklyRewardTier
}

// WeeklyRewards is the viewer's progress for a channel's Weekly Rewards event.
type WeeklyRewards struct {
	DaysVisitedThisWeek           int
	AccumulatedWeeks              int
	HasEarnedWeeklyRewardThisWeek bool
	HasVisitedToday               bool
	// CurrentReward is the next reward tier to be earned.
	CurrentReward WeeklyRewardTier
	EventConfig   WeeklyRewardsEventConfig
}

// WeeklyRewardsNotification is a weekly-rewards PubSub message.
type WeeklyRewardsNotification struct {
	ViewerID            string
	ChannelID           string
	EventID             string
	DaysVisitedThisWeek int
	// AccumulatedWeeks is nil when Twitch omits it (no completed weeks yet).
	AccumulatedWeeks    *int
	NotificationType    string
	CurrentReward       WeeklyRewardTier
	DaysRequiredPerWeek int
}

const WeeklyRewardsNotificationWeekCompleted = "WEEK_COMPLETED"

// Clip is a channel clip that can be "watched" to make Weekly Rewards progress.
type Clip struct {
	ID              string
	Slug            string
	URL             string
	Title           string
	DurationSeconds int
}

// VOD is a channel past broadcast that can be "watched" to make Weekly Rewards progress.
type VOD struct {
	ID            string
	BroadcastID   string
	LengthSeconds int
	PublishedAt   time.Time
}

// SetWeeklyRewards replaces the stored Weekly Rewards state; nil means the channel has no active event.
func (s *Streamer) SetWeeklyRewards(rewards *WeeklyRewards) {
	s.weeklyMu.Lock()
	defer s.weeklyMu.Unlock()
	if rewards == nil {
		s.weeklyRewards = nil
		return
	}
	copied := *rewards
	s.weeklyRewards = &copied
}

// WeeklyRewardsSnapshot returns a copy of the stored Weekly Rewards state.
func (s *Streamer) WeeklyRewardsSnapshot() (WeeklyRewards, bool) {
	s.weeklyMu.Lock()
	defer s.weeklyMu.Unlock()
	if s.weeklyRewards == nil {
		return WeeklyRewards{}, false
	}
	return *s.weeklyRewards, true
}

// MissingWeeklyReward reports whether a visit today would still make Weekly Rewards progress.
func (s *Streamer) MissingWeeklyReward(now time.Time) bool {
	if s == nil || !s.Settings.WeeklyRewards {
		return false
	}
	s.weeklyMu.Lock()
	defer s.weeklyMu.Unlock()
	wr := s.weeklyRewards
	if wr == nil {
		return false
	}
	if !wr.EventConfig.EndDate.IsZero() && !now.Before(wr.EventConfig.EndDate) {
		return false
	}
	// ? Nothing left to earn once every tier has been completed.
	if wr.AccumulatedWeeks >= len(wr.EventConfig.RewardTiers) {
		return false
	}
	return !wr.HasEarnedWeeklyRewardThisWeek && !wr.HasVisitedToday
}

// ApplyWeeklyRewardsNotification merges a PubSub notification into the stored state.
// ? It returns the updated state, or false when there is no state to update yet.
func (s *Streamer) ApplyWeeklyRewardsNotification(n WeeklyRewardsNotification) (WeeklyRewards, bool) {
	s.weeklyMu.Lock()
	defer s.weeklyMu.Unlock()
	wr := s.weeklyRewards
	if wr == nil {
		return WeeklyRewards{}, false
	}
	wr.DaysVisitedThisWeek = n.DaysVisitedThisWeek
	// ? Every notification is the result of a visit, so today is done.
	wr.HasVisitedToday = true
	if n.NotificationType == WeeklyRewardsNotificationWeekCompleted {
		wr.HasEarnedWeeklyRewardThisWeek = true
	}
	if n.AccumulatedWeeks != nil {
		wr.AccumulatedWeeks = *n.AccumulatedWeeks
	} else {
		wr.AccumulatedWeeks = 0
	}
	wr.CurrentReward.Tier = n.CurrentReward.Tier
	wr.CurrentReward.ChannelPoints = n.CurrentReward.ChannelPoints
	wr.CurrentReward.Badge.SetID = n.CurrentReward.Badge.SetID
	wr.CurrentReward.Badge.Version = n.CurrentReward.Badge.Version
	if n.DaysRequiredPerWeek > 0 {
		wr.EventConfig.DaysRequiredPerWeek = n.DaysRequiredPerWeek
	}
	return *wr, true
}

// SetWeeklyRewardsMedia stores the Clips and VODs that can be used to make progress.
func (s *Streamer) SetWeeklyRewardsMedia(clips []Clip, vods []VOD) {
	s.weeklyMu.Lock()
	defer s.weeklyMu.Unlock()
	s.weeklyClips = append([]Clip(nil), clips...)
	s.weeklyVODs = append([]VOD(nil), vods...)
}

// WeeklyRewardsMedia returns copies of the stored Clips and VODs.
func (s *Streamer) WeeklyRewardsMedia() ([]Clip, []VOD) {
	s.weeklyMu.Lock()
	defer s.weeklyMu.Unlock()
	return append([]Clip(nil), s.weeklyClips...), append([]VOD(nil), s.weeklyVODs...)
}
