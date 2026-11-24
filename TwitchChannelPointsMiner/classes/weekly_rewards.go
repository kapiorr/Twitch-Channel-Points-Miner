package classes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"TwitchChannelPointsMiner/TwitchChannelPointsMiner/classes/entities"
	"TwitchChannelPointsMiner/TwitchChannelPointsMiner/constants"
)

// ? Weekly Rewards support, ported from mpforce1/Twitch-Channel-Points-Miner PR #29.

const (
	weeklyRewardsClipsLimit = 10
	weeklyRewardsVODsLimit  = 5
	// ? Same fallback the streak recovery uses when the channel page can't be scraped.
	weeklyRewardsSpadeFallbackURL = "https://spade.twitch.tv/track"
)

// ? Intervals are variables so tests can shorten them.
var (
	clipPlaybackInterval = 5 * time.Second
	vodPlaybackInterval  = 60 * time.Second
	vodRetryInterval     = 5 * time.Second
	weeklyConditionPoll  = time.Second
)

type gqlResponseError struct {
	Message string `json:"message"`
}

func gqlErrorsToError(op string, errs []gqlResponseError) error {
	if len(errs) == 0 {
		return nil
	}
	msgs := make([]string, 0, len(errs))
	for _, e := range errs {
		msgs = append(msgs, e.Message)
	}
	return fmt.Errorf("%s: %s", op, strings.Join(msgs, "; "))
}

type gqlWeeklyRewardBadge struct {
	SetID   string `json:"setID"`
	Version string `json:"version"`
	Title   string `json:"title"`
}

type gqlWeeklyRewardTier struct {
	Tier          int                   `json:"tier"`
	ChannelPoints int                   `json:"channelPoints"`
	Badge         *gqlWeeklyRewardBadge `json:"badge"`
}

func (g gqlWeeklyRewardTier) toEntity() entities.WeeklyRewardTier {
	tier := entities.WeeklyRewardTier{Tier: g.Tier, ChannelPoints: g.ChannelPoints}
	if g.Badge != nil {
		tier.Badge = entities.WeeklyRewardBadge{SetID: g.Badge.SetID, Version: g.Badge.Version, Title: g.Badge.Title}
	}
	return tier
}

type gqlWeeklyVisitRewards struct {
	DaysVisitedThisWeek           int                 `json:"daysVisitedThisWeek"`
	AccumulatedWeeks              int                 `json:"accumulatedWeeks"`
	HasEarnedWeeklyRewardThisWeek bool                `json:"hasEarnedWeeklyRewardThisWeek"`
	HasVisitedToday               bool                `json:"hasVisitedToday"`
	CurrentReward                 gqlWeeklyRewardTier `json:"currentReward"`
	EventConfig                   struct {
		ID                  string                `json:"id"`
		DaysRequiredPerWeek int                   `json:"daysRequiredPerWeek"`
		EndDate             string                `json:"endDate"`
		WeekResetDates      []string              `json:"weekResetDates"`
		RewardTiers         []gqlWeeklyRewardTier `json:"rewardTiers"`
	} `json:"eventConfig"`
}

type gqlWeeklyRewardsResponse struct {
	Errors []gqlResponseError `json:"errors"`
	Data   struct {
		Channel *struct {
			Self *struct {
				WeeklyVisitRewards *gqlWeeklyVisitRewards `json:"weeklyVisitRewards"`
			} `json:"self"`
		} `json:"channel"`
	} `json:"data"`
}

// FetchWeeklyRewards loads the viewer's Weekly Rewards progress for a channel.
// ? It returns nil, nil when the channel has no Weekly Rewards event running.
func (t *Twitch) FetchWeeklyRewards(channelID string) (*entities.WeeklyRewards, error) {
	if strings.TrimSpace(channelID) == "" {
		return nil, errors.New("missing channel id")
	}
	op := constants.ClonePersistedOperation(constants.GQLOperations.WeeklyVisitRewardsQuery)
	op.Variables = map[string]interface{}{"channelID": channelID}
	var resp gqlWeeklyRewardsResponse
	if err := t.PostGQLDecode(op, &resp); err != nil {
		return nil, err
	}
	if resp.Data.Channel == nil || resp.Data.Channel.Self == nil {
		if err := gqlErrorsToError("WeeklyVisitRewardsQuery", resp.Errors); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("WeeklyVisitRewardsQuery: missing data.channel.self for channel %s", channelID)
	}
	raw := resp.Data.Channel.Self.WeeklyVisitRewards
	if raw == nil {
		return nil, nil
	}
	wr := &entities.WeeklyRewards{
		DaysVisitedThisWeek:           raw.DaysVisitedThisWeek,
		AccumulatedWeeks:              raw.AccumulatedWeeks,
		HasEarnedWeeklyRewardThisWeek: raw.HasEarnedWeeklyRewardThisWeek,
		HasVisitedToday:               raw.HasVisitedToday,
		CurrentReward:                 raw.CurrentReward.toEntity(),
		EventConfig: entities.WeeklyRewardsEventConfig{
			ID:                  raw.EventConfig.ID,
			DaysRequiredPerWeek: raw.EventConfig.DaysRequiredPerWeek,
			EndDate:             parseRFC3339Timestamp(raw.EventConfig.EndDate),
		},
	}
	for _, d := range raw.EventConfig.WeekResetDates {
		if ts := parseRFC3339Timestamp(d); !ts.IsZero() {
			wr.EventConfig.WeekResetDates = append(wr.EventConfig.WeekResetDates, ts)
		}
	}
	for _, tier := range raw.EventConfig.RewardTiers {
		wr.EventConfig.RewardTiers = append(wr.EventConfig.RewardTiers, tier.toEntity())
	}
	return wr, nil
}

type gqlClipsResponse struct {
	Errors []gqlResponseError `json:"errors"`
	Data   struct {
		User *struct {
			Clips *struct {
				Edges []struct {
					Node *struct {
						ID              string `json:"id"`
						Slug            string `json:"slug"`
						URL             string `json:"url"`
						Title           string `json:"title"`
						DurationSeconds int    `json:"durationSeconds"`
					} `json:"node"`
				} `json:"edges"`
			} `json:"clips"`
		} `json:"user"`
	} `json:"data"`
}

type gqlVideosResponse struct {
	Errors []gqlResponseError `json:"errors"`
	Data   struct {
		User *struct {
			Videos *struct {
				Edges []struct {
					Node *struct {
						ID                  string `json:"id"`
						LengthSeconds       int    `json:"lengthSeconds"`
						PublishedAt         string `json:"publishedAt"`
						BroadcastIdentifier *struct {
							ID string `json:"id"`
						} `json:"broadcastIdentifier"`
					} `json:"node"`
				} `json:"edges"`
			} `json:"videos"`
		} `json:"user"`
	} `json:"data"`
}

// FetchWeeklyRewardsMedia loads the channel's top Clips and most recent VODs.
func (t *Twitch) FetchWeeklyRewardsMedia(streamer *entities.Streamer) ([]entities.Clip, []entities.VOD, error) {
	if streamer == nil || strings.TrimSpace(streamer.Username) == "" {
		return nil, nil, errors.New("missing streamer login")
	}
	clips, clipsErr := t.fetchClips(streamer.Username)
	vods, vodsErr := t.fetchVODs(streamer.Username)
	return clips, vods, errors.Join(clipsErr, vodsErr)
}

func (t *Twitch) fetchClips(login string) ([]entities.Clip, error) {
	op := constants.ClonePersistedOperation(constants.GQLOperations.ClipsCardsUser)
	if op.Variables == nil {
		op.Variables = map[string]interface{}{}
	}
	op.Variables["login"] = login
	op.Variables["limit"] = weeklyRewardsClipsLimit
	op.Variables["criteria"] = map[string]interface{}{"filter": "ALL_TIME"}
	var resp gqlClipsResponse
	if err := t.PostGQLDecode(op, &resp); err != nil {
		return nil, fmt.Errorf("clips: %w", err)
	}
	if resp.Data.User == nil || resp.Data.User.Clips == nil {
		if err := gqlErrorsToError("ClipsCards__User", resp.Errors); err != nil {
			return nil, err
		}
		return nil, errors.New("clips: missing data.user.clips")
	}
	clips := make([]entities.Clip, 0, len(resp.Data.User.Clips.Edges))
	for _, edge := range resp.Data.User.Clips.Edges {
		if edge.Node == nil || edge.Node.ID == "" {
			continue
		}
		clips = append(clips, entities.Clip{
			ID:              edge.Node.ID,
			Slug:            edge.Node.Slug,
			URL:             edge.Node.URL,
			Title:           edge.Node.Title,
			DurationSeconds: edge.Node.DurationSeconds,
		})
	}
	return clips, nil
}

func (t *Twitch) fetchVODs(login string) ([]entities.VOD, error) {
	op := constants.ClonePersistedOperation(constants.GQLOperations.FilterableVideoTowerVideos)
	if op.Variables == nil {
		op.Variables = map[string]interface{}{}
	}
	op.Variables["channelOwnerLogin"] = login
	op.Variables["limit"] = weeklyRewardsVODsLimit
	op.Variables["videoSort"] = "TIME"
	// ? Past broadcasts only (no highlights/uploads), as in the Python miner.
	op.Variables["broadcastType"] = "ARCHIVE"
	op.Variables["includePreviewBlur"] = false
	var resp gqlVideosResponse
	if err := t.PostGQLDecode(op, &resp); err != nil {
		return nil, fmt.Errorf("videos: %w", err)
	}
	if resp.Data.User == nil || resp.Data.User.Videos == nil {
		if err := gqlErrorsToError("FilterableVideoTower_Videos", resp.Errors); err != nil {
			return nil, err
		}
		return nil, errors.New("videos: missing data.user.videos")
	}
	vods := make([]entities.VOD, 0, len(resp.Data.User.Videos.Edges))
	for _, edge := range resp.Data.User.Videos.Edges {
		if edge.Node == nil || edge.Node.ID == "" {
			continue
		}
		vod := entities.VOD{
			ID:            edge.Node.ID,
			LengthSeconds: edge.Node.LengthSeconds,
			PublishedAt:   parseRFC3339Timestamp(edge.Node.PublishedAt),
		}
		if edge.Node.BroadcastIdentifier != nil {
			vod.BroadcastID = edge.Node.BroadcastIdentifier.ID
		}
		vods = append(vods, vod)
	}
	return vods, nil
}

// VODViewable reports whether the viewer can play a VOD (subscriber-only VODs are not viewable).
func (t *Twitch) VODViewable(streamer *entities.Streamer, vodID string) (bool, error) {
	if streamer == nil || vodID == "" {
		return false, errors.New("missing streamer or vod id")
	}
	op := constants.ClonePersistedOperation(constants.GQLOperations.PlaybackAccessToken)
	op.Variables = map[string]interface{}{
		"isLive": false, "isVod": true, "login": streamer.Username,
		"platform": "web", "playerType": "site", "vodID": vodID,
	}
	var response struct {
		Errors []gqlResponseError `json:"errors"`
		Data   struct {
			Token *struct {
				Value     string `json:"value"`
				Signature string `json:"signature"`
			} `json:"videoPlaybackAccessToken"`
		} `json:"data"`
	}
	if err := t.PostGQLDecode(op, &response); err != nil {
		return false, err
	}
	token := response.Data.Token
	if token == nil || token.Value == "" || token.Signature == "" {
		if err := gqlErrorsToError("PlaybackAccessToken", response.Errors); err != nil {
			return false, err
		}
		return false, errors.New("vod playback token unavailable")
	}
	playlistURL := "https://usher.ttvnw.net/vod/v2/" + url.PathEscape(vodID) + ".m3u8?" +
		url.Values{"sig": {token.Signature}, "token": {token.Value}}.Encode()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, playlistURL, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("User-Agent", t.userAgent)
	resp, err := t.client.Do(req)
	if err != nil {
		return false, err
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return true, nil
	}
	if t.anonymizer != nil && t.anonymizer.Enabled() {
		t.debugf("VOD playlist status %d: %s", resp.StatusCode, usherErrorMessage(body))
	} else {
		t.debugf("VOD %s playlist status %d: %s", vodID, resp.StatusCode, usherErrorMessage(body))
	}
	return false, nil
}

// ? usher answers non-viewable VODs with e.g. [{"type":"error","error":"..."}].
func usherErrorMessage(body []byte) string {
	var items []struct {
		Type  string `json:"type"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &items); err == nil {
		for _, item := range items {
			if item.Type == "error" && item.Error != "" {
				return item.Error
			}
		}
	}
	return strings.TrimSpace(string(body))
}

func (t *Twitch) weeklyStreamerName(streamer *entities.Streamer) string {
	if t.anonymizer != nil && t.anonymizer.Enabled() {
		return t.anonymizer.StreamerName(streamer)
	}
	return displayName(streamer.Username)
}

func (t *Twitch) weeklySpadeURL(streamer *entities.Streamer) string {
	if streamer.Stream != nil && streamer.Stream.SpadeURL != "" {
		return streamer.Stream.SpadeURL
	}
	if err := t.GetSpadeURL(streamer); err == nil && streamer.Stream != nil && streamer.Stream.SpadeURL != "" {
		return streamer.Stream.SpadeURL
	} else if err != nil {
		t.debugf("Weekly Rewards: spade url lookup failed, using fallback: %v", err)
	}
	return weeklyRewardsSpadeFallbackURL
}

// ? waitWhile sleeps for d, polling keepGoing; false means it stopped early (cancelled or condition changed).
func waitWhile(d time.Duration, cancel <-chan struct{}, keepGoing func() bool) bool {
	if d <= 0 {
		return keepGoing()
	}
	deadline := time.Now().Add(d)
	for {
		if !keepGoing() {
			return false
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return true
		}
		step := weeklyConditionPoll
		if remaining < step {
			step = remaining
		}
		timer := time.NewTimer(step)
		select {
		case <-cancel:
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
}

func isCancelled(cancel <-chan struct{}) bool {
	select {
	case <-cancel:
		return true
	default:
		return false
	}
}

// SimulateClipPlayback reports a Clip as being watched until stillMissing turns false, maxWatch
// ? elapses (capped at the clip length) or cancel is closed. It returns true once progress is made.
func (t *Twitch) SimulateClipPlayback(streamer *entities.Streamer, clip entities.Clip, maxWatch time.Duration, cancel <-chan struct{}, stillMissing func() bool) bool {
	if clipLength := time.Duration(clip.DurationSeconds) * time.Second; clipLength > 0 && clipLength < maxWatch {
		maxWatch = clipLength
	}
	if t.logger != nil {
		t.logger.EmojiPrintf(":paperclip:", "Weekly Rewards %s: watching a clip for up to %s", t.weeklyStreamerName(streamer), maxWatch.Round(time.Second))
	}
	if t.anonymizer == nil || !t.anonymizer.Enabled() {
		t.debugf("Weekly Rewards: clip %s (%q, %ds)", clip.Slug, clip.Title, clip.DurationSeconds)
	}
	spadeURL := t.weeklySpadeURL(streamer)
	playSessionID := strings.ToLower(randomString(32))
	props := map[string]interface{}{
		"location":        "vod",
		"url":             clip.URL,
		"channel_id":      streamer.ChannelID,
		"vod_type":        "clip",
		"vod_id":          clip.ID,
		"content_mode":    "clip",
		"live":            false,
		"minutes_logged":  0,
		"play_session_id": playSessionID,
		"player":          "site",
		"user_id":         t.twitchLogin.UserID(),
		"vod_timestamp":   0,
		"clip_slug":       clip.Slug,
	}
	if err := t.sendSpadePayload(spadeURL, streamer.Username, []map[string]interface{}{{"event": "video-play", "properties": props}}); err != nil {
		t.debugf("Weekly Rewards: clip video-play failed: %v", err)
	}
	started := time.Now()
	secondsWatched := int(clipPlaybackInterval / time.Second)
	if secondsWatched <= 0 {
		secondsWatched = 1
	}
	step := secondsWatched
	for stillMissing() && time.Since(started) < maxWatch {
		if !waitWhile(clipPlaybackInterval, cancel, stillMissing) {
			break
		}
		secondProps := make(map[string]interface{}, len(props)+2)
		for k, v := range props {
			secondProps[k] = v
		}
		delete(secondProps, "content_mode")
		secondProps["platform"] = "web"
		secondProps["seconds_after_play"] = secondsWatched
		secondProps["vod_timestamp"] = float64(secondsWatched) - 0.1
		if err := t.sendSpadePayload(spadeURL, streamer.Username, []map[string]interface{}{{"event": "n_second_play", "properties": secondProps}}); err != nil {
			t.debugf("Weekly Rewards: clip n_second_play failed: %v", err)
		}
		secondsWatched += step
	}
	return !stillMissing()
}

// SimulateVODPlayback reports a VOD as being watched minute by minute until stillMissing turns false,
// ? maxWatch elapses or cancel is closed. It returns true once progress is made.
func (t *Twitch) SimulateVODPlayback(streamer *entities.Streamer, vod entities.VOD, maxWatch time.Duration, cancel <-chan struct{}, stillMissing func() bool) bool {
	if t.logger != nil {
		t.logger.EmojiPrintf(":clapper:", "Weekly Rewards %s: watching a VOD for up to %s", t.weeklyStreamerName(streamer), maxWatch.Round(time.Second))
	}
	spadeURL := t.weeklySpadeURL(streamer)
	payload := []map[string]interface{}{{
		"event": "minute-watched",
		"properties": map[string]interface{}{
			"channel_id":   streamer.ChannelID,
			"broadcast_id": nil,
			"player":       "site",
			"user_id":      t.twitchLogin.UserID(),
			"live":         false,
			"channel":      streamer.Username,
			"vod_id":       vod.ID,
			"content_mode": "vod",
		},
	}}
	accepted := 0
	started := time.Now()
	for time.Since(started) <= maxWatch && !isCancelled(cancel) {
		if !stillMissing() {
			break
		}
		requestStarted := time.Now()
		wait := vodRetryInterval
		if err := t.sendSpadePayload(spadeURL, streamer.Username, payload); err == nil {
			accepted++
			wait = vodPlaybackInterval
		} else {
			t.debugf("Weekly Rewards: VOD minute-watched failed: %v", err)
		}
		wait -= time.Since(requestStarted)
		if wait < time.Second && vodPlaybackInterval >= time.Second {
			wait = time.Second
		}
		if !waitWhile(wait, cancel, stillMissing) {
			break
		}
	}
	t.debugf("Weekly Rewards: sent %d VOD minute-watched events in %s", accepted, time.Since(started).Round(time.Second))
	return !stillMissing()
}

type weeklyRewardsNotificationPayload struct {
	ViewerID            string `json:"viewerId"`
	ChannelID           string `json:"channelId"`
	EventID             string `json:"eventId"`
	DaysVisitedThisWeek int    `json:"daysVisitedThisWeek"`
	AccumulatedWeeks    *int   `json:"accumulatedWeeks"`
	NotificationType    string `json:"notificationType"`
	CurrentReward       *struct {
		Tier          int    `json:"tier"`
		ChannelPoints int    `json:"channelPoints"`
		BadgeSetID    string `json:"badgeSetId"`
		BadgeVersion  string `json:"badgeVersion"`
	} `json:"currentReward"`
	EventConfig *struct {
		DaysRequiredPerWeek int `json:"daysRequiredPerWeek"`
	} `json:"eventConfig"`
}

// ParseWeeklyRewardsNotification decodes a weekly-rewards PubSub message.
func ParseWeeklyRewardsNotification(raw []byte) (entities.WeeklyRewardsNotification, error) {
	var payload weeklyRewardsNotificationPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return entities.WeeklyRewardsNotification{}, err
	}
	// ? Tolerate a {"type":...,"data":{...}} envelope, like other PubSub topics use.
	if payload.ChannelID == "" && payload.NotificationType == "" {
		var envelope struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(raw, &envelope); err == nil && len(envelope.Data) > 0 && envelope.Data[0] == '{' {
			if err := json.Unmarshal(envelope.Data, &payload); err != nil {
				return entities.WeeklyRewardsNotification{}, err
			}
		}
	}
	if payload.NotificationType == "" {
		return entities.WeeklyRewardsNotification{}, errors.New("weekly-rewards message without notificationType")
	}
	n := entities.WeeklyRewardsNotification{
		ViewerID:            payload.ViewerID,
		ChannelID:           payload.ChannelID,
		EventID:             payload.EventID,
		DaysVisitedThisWeek: payload.DaysVisitedThisWeek,
		AccumulatedWeeks:    payload.AccumulatedWeeks,
		NotificationType:    payload.NotificationType,
	}
	if payload.CurrentReward != nil {
		n.CurrentReward = entities.WeeklyRewardTier{
			Tier:          payload.CurrentReward.Tier,
			ChannelPoints: payload.CurrentReward.ChannelPoints,
			Badge: entities.WeeklyRewardBadge{
				SetID:   payload.CurrentReward.BadgeSetID,
				Version: payload.CurrentReward.BadgeVersion,
			},
		}
	}
	if payload.EventConfig != nil {
		n.DaysRequiredPerWeek = payload.EventConfig.DaysRequiredPerWeek
	}
	return n, nil
}
