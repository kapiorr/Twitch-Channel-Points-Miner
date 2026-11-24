package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	miner "TwitchChannelPointsMiner/TwitchChannelPointsMiner"
	"TwitchChannelPointsMiner/TwitchChannelPointsMiner/classes/entities"
)

func TestDefaultConfigIncludesExpectedKeys(t *testing.T) {
	cfg := defaultConfig()
	required := []string{
		"username",
		"password",
		"claim_moments",
		"streamers",
		"game_priority",
		"chat_presence",
		"disable_at_in_nickname",
		"show_drops_progress",
		"bet",
		"watch_priority",
	}
	for _, key := range required {
		if _, ok := cfg[key]; !ok {
			t.Fatalf("missing key %q in default config", key)
		}
	}
	if got, ok := cfg["show_drops_progress"].(bool); !ok || got {
		t.Fatalf("show_drops_progress default got %#v, want false", cfg["show_drops_progress"])
	}
}

func TestLoadOrCreateConfigCreatesFileAndDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")

	cfg, err := loadOrCreateConfig(path)
	if err != nil {
		t.Fatalf("load/create config error: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config file should be created: %v", err)
	}
	if cfg.WatchPriority == nil || len(cfg.WatchPriority) == 0 {
		t.Fatalf("watch priority should have defaults: %#v", cfg)
	}
}

func TestApplyTimezoneOverride(t *testing.T) {
	original := time.Local
	defer func() { time.Local = original }()

	zone := "UTC"
	logger := miner.NewLogger(miner.LoggerSettings{}, "")
	applyTimezoneOverride(&zone, logger)
	if time.Local.String() != "UTC" {
		t.Fatalf("expected time.Local set to UTC, got %s", time.Local.String())
	}

	bad := "Bad/Zone"
	applyTimezoneOverride(&bad, logger)
	if time.Local.String() != "UTC" {
		t.Fatalf("invalid timezone should not change location, got %s", time.Local.String())
	}
}

func TestBuildBaseStreamerSettingsAppliesGlobalFilterCondition(t *testing.T) {
	cfg := config{
		BettingMakePredictions: true,
		FollowRaid:             true,
		ClaimDrops:             true,
		CommunityGoals:         false,
		IRCMode:                "ONLINE",
		Bet: betConfig{
			FilterCondition: &filterConditionConfig{
				By:    "TOTAL_USERS",
				Where: "GTE",
				Value: func() *float64 { v := 500000.0; return &v }(),
			},
		},
	}

	base := buildBaseStreamerSettings(cfg)
	if base.Bet.FilterCondition == nil {
		t.Fatalf("expected global filter_condition applied to base streamer settings")
	}
	if base.Bet.FilterCondition.By != "TOTAL_USERS" {
		t.Fatalf("expected By TOTAL_USERS, got %s", base.Bet.FilterCondition.By)
	}
	if base.Bet.FilterCondition.Where != "GTE" {
		t.Fatalf("expected Where GTE, got %s", base.Bet.FilterCondition.Where)
	}
	if base.Bet.FilterCondition.Value == nil || *base.Bet.FilterCondition.Value != 500000.0 {
		t.Fatalf("expected Value 500000, got %#v", base.Bet.FilterCondition.Value)
	}
}

func TestBuildBaseStreamerSettingsUsesGlobalClaimMoments(t *testing.T) {
	cfg := config{
		BettingMakePredictions: true,
		FollowRaid:             true,
		ClaimDrops:             true,
		ClaimMoments:           false,
		CommunityGoals:         false,
		IRCMode:                "ONLINE",
	}

	base := buildBaseStreamerSettings(cfg)
	if base.ClaimMoments {
		t.Fatalf("expected base claim_moments false from global config")
	}
}

func TestBuildOverrideSettingsMergesFilterCondition(t *testing.T) {
	base := entities.StreamerSettings{
		MakePredictions: true,
		Bet: entities.BetSettings{
			Strategy: entities.StrategySmart,
		},
	}
	base.Default()

	overrides := map[string]streamerSettingsConfig{
		"SomeStreamer": {
			Bet: betConfig{
				FilterCondition: &filterConditionConfig{
					By:    "TOTAL_POINTS",
					Where: "GT",
					Value: func() *float64 { v := 999999.0; return &v }(),
				},
			},
		},
	}

	merged := buildOverrideSettings(base, overrides)
	override, ok := merged["somestreamer"]
	if !ok {
		t.Fatalf("expected override settings keyed by lowercased streamer name")
	}
	if override.Bet.FilterCondition == nil {
		t.Fatalf("expected override filter_condition merged into settings")
	}
	if override.Bet.FilterCondition.By != "TOTAL_POINTS" || override.Bet.FilterCondition.Where != "GT" {
		t.Fatalf("expected override filter_condition TOTAL_POINTS GT, got %v %v", override.Bet.FilterCondition.By, override.Bet.FilterCondition.Where)
	}
	if override.Bet.FilterCondition.Value == nil || *override.Bet.FilterCondition.Value != 999999.0 {
		t.Fatalf("expected override Value 999999, got %#v", override.Bet.FilterCondition.Value)
	}
}

func TestBuildOverrideSettingsCanEnableClaimMomentsOverGlobalDefault(t *testing.T) {
	base := entities.StreamerSettings{
		ClaimMoments: false,
	}
	base.Default()

	enable := true
	overrides := map[string]streamerSettingsConfig{
		"SomeStreamer": {
			ClaimMoments: &enable,
		},
	}

	merged := buildOverrideSettings(base, overrides)
	got, ok := merged["somestreamer"]
	if !ok {
		t.Fatalf("expected override for somestreamer")
	}
	if !got.ClaimMoments {
		t.Fatalf("expected streamer override to enable claim_moments")
	}
}

func TestDefaultConfigIncludesWeeklyRewards(t *testing.T) {
	cfg := defaultConfig()
	if got, ok := cfg["weekly_rewards"].(bool); !ok || !got {
		t.Fatalf("weekly_rewards default got %#v, want true", cfg["weekly_rewards"])
	}
	progressor, ok := cfg["weekly_rewards_progressor"].(map[string]interface{})
	if !ok {
		t.Fatalf("weekly_rewards_progressor default missing")
	}
	for _, key := range []string{"enabled", "max_concurrent_watch", "max_seconds_clips", "max_seconds_vods", "loop_interval_seconds", "max_failures_per_streamer", "failure_cooldown_seconds"} {
		if _, ok := progressor[key]; !ok {
			t.Fatalf("weekly_rewards_progressor missing %q", key)
		}
	}
}

func TestLoadOrCreateConfigFillsWeeklyRewardsDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	// ? An existing config from before Weekly Rewards, with a partial progressor block.
	raw := `{"username":"viewer","weekly_rewards_progressor":{"max_concurrent_watch":5}}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := loadOrCreateConfig(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if !cfg.WeeklyRewards {
		t.Fatalf("weekly_rewards should default to true")
	}
	settings := buildWeeklyRewardsProgressorSettings(cfg.WeeklyRewardsProgressor)
	if !settings.Enabled || settings.MaxConcurrentWatch != 5 || settings.MaxSecondsVODs != 480 || settings.FailureCooldownSeconds != 3600 {
		t.Fatalf("unexpected progressor settings: %+v", settings)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	for _, key := range []string{`"weekly_rewards"`, `"max_seconds_clips"`, `"failure_cooldown_seconds"`} {
		if !strings.Contains(string(written), key) {
			t.Fatalf("missing %s in the rewritten config", key)
		}
	}
}

func TestBuildWeeklyRewardsProgressorSettings(t *testing.T) {
	disabled := false
	zero := 0
	clips := 45.0
	got := buildWeeklyRewardsProgressorSettings(weeklyRewardsProgressorConfig{
		Enabled:            &disabled,
		MaxConcurrentWatch: &zero,
		MaxSecondsClips:    &clips,
	})
	def := miner.DefaultWeeklyRewardsProgressorSettings()
	if got.Enabled || got.MaxSecondsClips != 45 || got.MaxConcurrentWatch != def.MaxConcurrentWatch {
		t.Fatalf("unexpected settings: %+v", got)
	}
	if got := buildWeeklyRewardsProgressorSettings(weeklyRewardsProgressorConfig{}); got != def {
		t.Fatalf("empty config should give defaults, got %+v", got)
	}
}

func TestWeeklyRewardsGlobalAndOverride(t *testing.T) {
	base := buildBaseStreamerSettings(config{WeeklyRewards: true})
	if !base.WeeklyRewards {
		t.Fatalf("global weekly_rewards should reach the base settings")
	}
	off := false
	merged := buildOverrideSettings(base, map[string]streamerSettingsConfig{"Quiet": {WeeklyRewards: &off}, "Other": {}})
	if merged["quiet"].WeeklyRewards {
		t.Fatalf("override should disable weekly_rewards for one streamer")
	}
	if !merged["other"].WeeklyRewards {
		t.Fatalf("streamers without an override keep the global value")
	}
}
