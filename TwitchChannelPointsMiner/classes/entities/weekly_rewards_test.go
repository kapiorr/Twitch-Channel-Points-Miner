package entities

import (
	"testing"
	"time"
)

func weeklyTestRewards() *WeeklyRewards {
	return &WeeklyRewards{
		DaysVisitedThisWeek: 1,
		AccumulatedWeeks:    0,
		CurrentReward:       WeeklyRewardTier{Tier: 1, ChannelPoints: 200},
		EventConfig: WeeklyRewardsEventConfig{
			ID:                  "event",
			DaysRequiredPerWeek: 3,
			RewardTiers: []WeeklyRewardTier{
				{Tier: 1, ChannelPoints: 200},
				{Tier: 2, ChannelPoints: 250},
				{Tier: 3, ChannelPoints: 300},
				{Tier: 4, ChannelPoints: 350},
			},
		},
	}
}

func TestMissingWeeklyReward(t *testing.T) {
	now := time.Date(2026, time.October, 23, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		enabled bool
		mutate  func(*WeeklyRewards) *WeeklyRewards
		want    bool
	}{
		{"missing today", true, func(w *WeeklyRewards) *WeeklyRewards { return w }, true},
		{"setting disabled", false, func(w *WeeklyRewards) *WeeklyRewards { return w }, false},
		{"no event", true, func(*WeeklyRewards) *WeeklyRewards { return nil }, false},
		{"visited today", true, func(w *WeeklyRewards) *WeeklyRewards { w.HasVisitedToday = true; return w }, false},
		{"week already earned", true, func(w *WeeklyRewards) *WeeklyRewards { w.HasEarnedWeeklyRewardThisWeek = true; return w }, false},
		{"all tiers completed", true, func(w *WeeklyRewards) *WeeklyRewards { w.AccumulatedWeeks = 4; return w }, false},
		{"event ended", true, func(w *WeeklyRewards) *WeeklyRewards { w.EventConfig.EndDate = now.Add(-time.Minute); return w }, false},
		{"event still running", true, func(w *WeeklyRewards) *WeeklyRewards { w.EventConfig.EndDate = now.Add(time.Hour); return w }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Streamer{Settings: StreamerSettings{WeeklyRewards: tc.enabled}}
			s.SetWeeklyRewards(tc.mutate(weeklyTestRewards()))
			if got := s.MissingWeeklyReward(now); got != tc.want {
				t.Fatalf("MissingWeeklyReward got %t want %t", got, tc.want)
			}
		})
	}
	var nilStreamer *Streamer
	if nilStreamer.MissingWeeklyReward(now) {
		t.Fatalf("nil streamer should not be missing a reward")
	}
}

func TestApplyWeeklyRewardsNotification(t *testing.T) {
	s := &Streamer{Settings: StreamerSettings{WeeklyRewards: true}}
	if _, ok := s.ApplyWeeklyRewardsNotification(WeeklyRewardsNotification{NotificationType: "DAY_COMPLETED"}); ok {
		t.Fatalf("notification without synced state should be ignored")
	}

	s.SetWeeklyRewards(weeklyTestRewards())
	weeks := 1
	updated, ok := s.ApplyWeeklyRewardsNotification(WeeklyRewardsNotification{
		DaysVisitedThisWeek: 2,
		AccumulatedWeeks:    &weeks,
		NotificationType:    "DAY_COMPLETED",
		CurrentReward:       WeeklyRewardTier{Tier: 2, ChannelPoints: 250, Badge: WeeklyRewardBadge{SetID: "set", Version: "2"}},
	})
	if !ok {
		t.Fatalf("expected the notification to be applied")
	}
	if updated.DaysVisitedThisWeek != 2 || !updated.HasVisitedToday || updated.HasEarnedWeeklyRewardThisWeek {
		t.Fatalf("unexpected state after day notification: %+v", updated)
	}
	if updated.AccumulatedWeeks != 1 || updated.CurrentReward.Tier != 2 || updated.CurrentReward.Badge.SetID != "set" {
		t.Fatalf("reward fields not updated: %+v", updated)
	}
	if updated.EventConfig.DaysRequiredPerWeek != 3 {
		t.Fatalf("days required should be kept when the notification omits it, got %d", updated.EventConfig.DaysRequiredPerWeek)
	}
	if s.MissingWeeklyReward(time.Now()) {
		t.Fatalf("a visit notification should mark today as done")
	}

	updated, _ = s.ApplyWeeklyRewardsNotification(WeeklyRewardsNotification{
		DaysVisitedThisWeek: 3,
		NotificationType:    WeeklyRewardsNotificationWeekCompleted,
		DaysRequiredPerWeek: 4,
	})
	if !updated.HasEarnedWeeklyRewardThisWeek {
		t.Fatalf("WEEK_COMPLETED should mark the week as earned")
	}
	if updated.AccumulatedWeeks != 0 {
		t.Fatalf("missing accumulatedWeeks should reset to 0, got %d", updated.AccumulatedWeeks)
	}
	if updated.EventConfig.DaysRequiredPerWeek != 4 {
		t.Fatalf("days required should follow the notification, got %d", updated.EventConfig.DaysRequiredPerWeek)
	}

	// ? Once earned, later notifications don't un-earn the week.
	updated, _ = s.ApplyWeeklyRewardsNotification(WeeklyRewardsNotification{NotificationType: "DAY_COMPLETED"})
	if !updated.HasEarnedWeeklyRewardThisWeek {
		t.Fatalf("earned week should stay earned")
	}
}

func TestWeeklyRewardsStateIsCopied(t *testing.T) {
	s := &Streamer{}
	rewards := weeklyTestRewards()
	s.SetWeeklyRewards(rewards)
	rewards.HasVisitedToday = true
	snap, ok := s.WeeklyRewardsSnapshot()
	if !ok || snap.HasVisitedToday {
		t.Fatalf("stored state should not follow later changes to the input")
	}

	clips := []Clip{{ID: "c1"}}
	vods := []VOD{{ID: "v1"}}
	s.SetWeeklyRewardsMedia(clips, vods)
	clips[0].ID = "changed"
	gotClips, gotVODs := s.WeeklyRewardsMedia()
	gotVODs[0].ID = "changed"
	_, again := s.WeeklyRewardsMedia()
	if gotClips[0].ID != "c1" || again[0].ID != "v1" {
		t.Fatalf("media should be copied on the way in and out")
	}

	s.SetWeeklyRewards(nil)
	if _, ok := s.WeeklyRewardsSnapshot(); ok {
		t.Fatalf("nil should clear the state")
	}
}
