package classes

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"TwitchChannelPointsMiner/TwitchChannelPointsMiner/classes/entities"
	"TwitchChannelPointsMiner/TwitchChannelPointsMiner/constants"
)

type weeklyGQLRequest struct {
	OperationName string                 `json:"operationName"`
	Variables     map[string]interface{} `json:"variables"`
	Extensions    struct {
		PersistedQuery struct {
			Sha256Hash string `json:"sha256Hash"`
		} `json:"persistedQuery"`
	} `json:"extensions"`
}

type spadeEvent struct {
	Event      string                 `json:"event"`
	Properties map[string]interface{} `json:"properties"`
}

type weeklyTestServer struct {
	mu       sync.Mutex
	gql      []weeklyGQLRequest
	usher    []*url.URL
	spade    []spadeEvent
	gqlReply func(req weeklyGQLRequest) string
	usherFn  func(u *url.URL) *http.Response
}

func (s *weeklyTestServer) spadeEvents() []spadeEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]spadeEvent(nil), s.spade...)
}

func newWeeklyTestTwitch(t *testing.T, srv *weeklyTestServer) *Twitch {
	t.Helper()
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch {
		case req.URL.Host == "gql.twitch.tv":
			body, _ := io.ReadAll(req.Body)
			var parsed weeklyGQLRequest
			if err := json.Unmarshal(body, &parsed); err != nil {
				t.Errorf("bad gql body: %v", err)
			}
			srv.mu.Lock()
			srv.gql = append(srv.gql, parsed)
			srv.mu.Unlock()
			return jsonResponse(200, srv.gqlReply(parsed)), nil
		case req.URL.Host == "usher.ttvnw.net":
			srv.mu.Lock()
			srv.usher = append(srv.usher, req.URL)
			srv.mu.Unlock()
			return srv.usherFn(req.URL), nil
		case req.Method == http.MethodPost && req.URL.Host == "spade.example":
			body, _ := io.ReadAll(req.Body)
			form, _ := url.ParseQuery(string(body))
			raw, err := base64.StdEncoding.DecodeString(form.Get("data"))
			if err != nil {
				t.Errorf("bad spade payload: %v", err)
			}
			var events []spadeEvent
			if err := json.Unmarshal(raw, &events); err != nil {
				t.Errorf("bad spade json: %v", err)
			}
			srv.mu.Lock()
			srv.spade = append(srv.spade, events...)
			srv.mu.Unlock()
			return jsonResponse(http.StatusNoContent, ""), nil
		default:
			t.Errorf("unexpected request: %s %s", req.Method, req.URL)
			return jsonResponse(400, ""), nil
		}
	})}
	return &Twitch{
		userAgent:      "ua",
		deviceID:       "device",
		clientSession:  "session",
		clientVersion:  constants.ClientVersion,
		versionTTL:     time.Hour,
		versionFetched: time.Now(),
		twitchLogin:    &TwitchLogin{Token: "token", userID: "viewer-1"},
		client:         client,
	}
}

func weeklyTestStreamer() *entities.Streamer {
	s := newTestStreamer(false)
	s.Settings.WeeklyRewards = true
	s.Stream.SpadeURL = "https://spade.example/track"
	return s
}

func shortenWeeklyIntervals(t *testing.T) {
	t.Helper()
	prevClip, prevVOD, prevRetry, prevPoll := clipPlaybackInterval, vodPlaybackInterval, vodRetryInterval, weeklyConditionPoll
	clipPlaybackInterval = 10 * time.Millisecond
	vodPlaybackInterval = 10 * time.Millisecond
	vodRetryInterval = 5 * time.Millisecond
	weeklyConditionPoll = 2 * time.Millisecond
	t.Cleanup(func() {
		clipPlaybackInterval, vodPlaybackInterval, vodRetryInterval, weeklyConditionPoll = prevClip, prevVOD, prevRetry, prevPoll
	})
}

const weeklyRewardsGQLFixture = `{"data":{"channel":{"id":"123456","self":{"weeklyVisitRewards":{
"daysVisitedThisWeek":1,"accumulatedWeeks":2,"hasEarnedWeeklyRewardThisWeek":false,"hasVisitedToday":false,
"currentReward":{"tier":3,"channelPoints":300,"badge":{"id":"b3","setID":"weekly","version":"3","title":"Week 3","image1x":"","image2x":"","image4x":"","clickAction":"","clickURL":""}},
"eventConfig":{"id":"event-1","daysRequiredPerWeek":3,"endDate":"2026-11-30T00:00:00Z",
"weekResetDates":["2026-10-27T00:00:00Z","not-a-date","2026-11-03T00:00:00Z"],
"rewardTiers":[{"tier":1,"channelPoints":200,"badge":{"setID":"weekly","version":"1","title":"Week 1"}},
{"tier":2,"channelPoints":250,"badge":{"setID":"weekly","version":"2","title":"Week 2"}},
{"tier":3,"channelPoints":300,"badge":{"setID":"weekly","version":"3","title":"Week 3"}},
{"tier":4,"channelPoints":350,"badge":null}]}}}}}}`

func TestFetchWeeklyRewardsParsesState(t *testing.T) {
	srv := &weeklyTestServer{gqlReply: func(weeklyGQLRequest) string { return weeklyRewardsGQLFixture }}
	tw := newWeeklyTestTwitch(t, srv)

	got, err := tw.FetchWeeklyRewards("123456")
	if err != nil {
		t.Fatalf("FetchWeeklyRewards error: %v", err)
	}
	if len(srv.gql) != 1 || srv.gql[0].OperationName != "WeeklyVisitRewardsQuery" || srv.gql[0].Variables["channelID"] != "123456" {
		t.Fatalf("unexpected request: %+v", srv.gql)
	}
	if srv.gql[0].Extensions.PersistedQuery.Sha256Hash != "ce98e9db55db7e4abcc1f5ac65c933b73c58fa9c4c8afe3c5098a8ed79737a3c" {
		t.Fatalf("unexpected persisted query hash %q", srv.gql[0].Extensions.PersistedQuery.Sha256Hash)
	}
	if got == nil || got.DaysVisitedThisWeek != 1 || got.AccumulatedWeeks != 2 || got.HasVisitedToday || got.HasEarnedWeeklyRewardThisWeek {
		t.Fatalf("unexpected progress: %+v", got)
	}
	if got.CurrentReward.Tier != 3 || got.CurrentReward.ChannelPoints != 300 || got.CurrentReward.Badge.Title != "Week 3" {
		t.Fatalf("unexpected current reward: %+v", got.CurrentReward)
	}
	cfg := got.EventConfig
	if cfg.ID != "event-1" || cfg.DaysRequiredPerWeek != 3 || !cfg.EndDate.Equal(time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("unexpected event config: %+v", cfg)
	}
	if len(cfg.WeekResetDates) != 2 || len(cfg.RewardTiers) != 4 || cfg.RewardTiers[3].ChannelPoints != 350 || cfg.RewardTiers[3].Badge.SetID != "" {
		t.Fatalf("unexpected dates/tiers: %+v", cfg)
	}
}

func TestFetchWeeklyRewardsWithoutEventAndErrors(t *testing.T) {
	reply := `{"data":{"channel":{"self":{"weeklyVisitRewards":null}}}}`
	srv := &weeklyTestServer{gqlReply: func(weeklyGQLRequest) string { return reply }}
	tw := newWeeklyTestTwitch(t, srv)
	got, err := tw.FetchWeeklyRewards("123456")
	if err != nil || got != nil {
		t.Fatalf("no event should give nil, nil; got %+v, %v", got, err)
	}

	reply = `{"errors":[{"message":"PersistedQueryNotFound"}],"data":null}`
	if _, err := tw.FetchWeeklyRewards("123456"); err == nil || !strings.Contains(err.Error(), "PersistedQueryNotFound") {
		t.Fatalf("expected the GQL error to be returned, got %v", err)
	}

	reply = `{"data":{"channel":null}}`
	if _, err := tw.FetchWeeklyRewards("123456"); err == nil {
		t.Fatalf("expected an error for a missing channel")
	}
	if _, err := tw.FetchWeeklyRewards(" "); err == nil {
		t.Fatalf("expected an error for an empty channel id")
	}
}

func TestFetchWeeklyRewardsMedia(t *testing.T) {
	srv := &weeklyTestServer{gqlReply: func(req weeklyGQLRequest) string {
		switch req.OperationName {
		case "ClipsCards__User":
			return `{"data":{"user":{"clips":{"edges":[
{"cursor":null,"node":{"id":"c1","slug":"Slug-One","url":"https://clips.twitch.tv/Slug-One","title":"One","durationSeconds":30}},
{"node":null},
{"node":{"id":"c2","slug":"Slug-Two","url":"https://clips.twitch.tv/Slug-Two","title":"Two","durationSeconds":4}}]}}}}`
		case "FilterableVideoTower_Videos":
			return `{"data":{"user":{"videos":{"edges":[
{"node":{"id":"v1","lengthSeconds":7200,"publishedAt":"2026-10-22T18:00:00Z","broadcastIdentifier":{"id":"b1"}}},
{"node":{"id":"v2","lengthSeconds":120,"publishedAt":"2026-10-21T18:00:00Z","broadcastIdentifier":null}}]}}}}`
		}
		t.Errorf("unexpected operation %s", req.OperationName)
		return `{}`
	}}
	tw := newWeeklyTestTwitch(t, srv)

	clips, vods, err := tw.FetchWeeklyRewardsMedia(weeklyTestStreamer())
	if err != nil {
		t.Fatalf("FetchWeeklyRewardsMedia error: %v", err)
	}
	if len(clips) != 2 || clips[0].ID != "c1" || clips[0].Slug != "Slug-One" || clips[0].DurationSeconds != 30 || clips[1].DurationSeconds != 4 {
		t.Fatalf("unexpected clips: %+v", clips)
	}
	if len(vods) != 2 || vods[0].ID != "v1" || vods[0].BroadcastID != "b1" || vods[0].LengthSeconds != 7200 || vods[1].BroadcastID != "" {
		t.Fatalf("unexpected vods: %+v", vods)
	}
	for _, req := range srv.gql {
		switch req.OperationName {
		case "ClipsCards__User":
			if req.Variables["login"] != "streamer" || req.Variables["limit"] != float64(weeklyRewardsClipsLimit) {
				t.Fatalf("unexpected clips variables: %+v", req.Variables)
			}
		case "FilterableVideoTower_Videos":
			v := req.Variables
			if v["channelOwnerLogin"] != "streamer" || v["limit"] != float64(weeklyRewardsVODsLimit) || v["broadcastType"] != "ARCHIVE" || v["includePreviewBlur"] != false {
				t.Fatalf("unexpected videos variables: %+v", v)
			}
		}
	}
	// ? The shared operation must not be mutated by per-request variables.
	if _, ok := constants.GQLOperations.FilterableVideoTowerVideos.Variables["broadcastType"]; ok {
		t.Fatalf("global FilterableVideoTower_Videos operation was mutated")
	}
}

func TestFetchWeeklyRewardsMediaKeepsPartialResults(t *testing.T) {
	srv := &weeklyTestServer{gqlReply: func(req weeklyGQLRequest) string {
		if req.OperationName == "ClipsCards__User" {
			return `{"data":{"user":{"clips":{"edges":[{"node":{"id":"c1","durationSeconds":30}}]}}}}`
		}
		return `{"errors":[{"message":"service error"}],"data":{"user":null}}`
	}}
	tw := newWeeklyTestTwitch(t, srv)
	clips, vods, err := tw.FetchWeeklyRewardsMedia(weeklyTestStreamer())
	if err == nil || !strings.Contains(err.Error(), "service error") {
		t.Fatalf("expected the videos error, got %v", err)
	}
	if len(clips) != 1 || len(vods) != 0 {
		t.Fatalf("clips should still be returned: %+v %+v", clips, vods)
	}
}

func TestVODViewable(t *testing.T) {
	srv := &weeklyTestServer{
		gqlReply: func(req weeklyGQLRequest) string {
			return `{"data":{"videoPlaybackAccessToken":{"value":"{\"a\":\"b&c\"}","signature":"sig"}}}`
		},
		usherFn: func(u *url.URL) *http.Response {
			if strings.HasSuffix(u.Path, "/sub-only.m3u8") {
				return jsonResponse(403, `[{"type":"error","error":"vod_manifest_restricted"}]`)
			}
			return jsonResponse(200, "#EXTM3U\n")
		},
	}
	tw := newWeeklyTestTwitch(t, srv)
	streamer := weeklyTestStreamer()

	ok, err := tw.VODViewable(streamer, "v1")
	if err != nil || !ok {
		t.Fatalf("expected viewable VOD, got %t %v", ok, err)
	}
	req := srv.gql[0]
	if req.OperationName != "PlaybackAccessToken" || req.Variables["isVod"] != true || req.Variables["isLive"] != false || req.Variables["vodID"] != "v1" || req.Variables["login"] != "streamer" {
		t.Fatalf("unexpected token request: %+v", req.Variables)
	}
	u := srv.usher[0]
	if u.Path != "/vod/v2/v1.m3u8" || u.Query().Get("token") != `{"a":"b&c"}` || u.Query().Get("sig") != "sig" {
		t.Fatalf("unexpected usher url: %s", u)
	}

	ok, err = tw.VODViewable(streamer, "sub-only")
	if err != nil || ok {
		t.Fatalf("restricted VOD should be not viewable without error, got %t %v", ok, err)
	}

	srv.gqlReply = func(weeklyGQLRequest) string { return `{"data":{"videoPlaybackAccessToken":null}}` }
	if ok, err := tw.VODViewable(streamer, "v2"); err == nil || ok {
		t.Fatalf("missing token should be an error, got %t %v", ok, err)
	}
}

func TestSimulateClipPlaybackStopsOnProgress(t *testing.T) {
	shortenWeeklyIntervals(t)
	srv := &weeklyTestServer{}
	tw := newWeeklyTestTwitch(t, srv)
	streamer := weeklyTestStreamer()
	clip := entities.Clip{ID: "c1", Slug: "Slug", URL: "https://clips.twitch.tv/Slug", DurationSeconds: 60}

	// ? Progress arrives after the second n_second_play event.
	stillMissing := func() bool {
		count := 0
		for _, e := range srv.spadeEvents() {
			if e.Event == "n_second_play" {
				count++
			}
		}
		return count < 2
	}
	if !tw.SimulateClipPlayback(streamer, clip, time.Second, make(chan struct{}), stillMissing) {
		t.Fatalf("expected success once progress is made")
	}
	events := srv.spadeEvents()
	if len(events) != 3 || events[0].Event != "video-play" || events[1].Event != "n_second_play" || events[2].Event != "n_second_play" {
		t.Fatalf("unexpected events: %+v", events)
	}
	play, second := events[0].Properties, events[1].Properties
	if play["vod_type"] != "clip" || play["vod_id"] != "c1" || play["clip_slug"] != "Slug" || play["channel_id"] != "123456" || play["user_id"] != "viewer-1" || play["content_mode"] != "clip" {
		t.Fatalf("unexpected video-play properties: %+v", play)
	}
	session, _ := play["play_session_id"].(string)
	if len(session) != 32 || session != strings.ToLower(session) || second["play_session_id"] != session {
		t.Fatalf("play session id should be a shared 32 char lowercase id, got %q / %v", session, second["play_session_id"])
	}
	if _, ok := second["content_mode"]; ok || second["platform"] != "web" || second["seconds_after_play"] == nil {
		t.Fatalf("unexpected n_second_play properties: %+v", second)
	}
}

func TestSimulateClipPlaybackTimesOutAndCancels(t *testing.T) {
	shortenWeeklyIntervals(t)
	srv := &weeklyTestServer{}
	tw := newWeeklyTestTwitch(t, srv)
	streamer := weeklyTestStreamer()
	always := func() bool { return true }

	started := time.Now()
	if tw.SimulateClipPlayback(streamer, entities.Clip{ID: "c1", DurationSeconds: 60}, 50*time.Millisecond, make(chan struct{}), always) {
		t.Fatalf("no progress should report failure")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("clip watching should stop at maxWatch, took %s", elapsed)
	}

	cancel := make(chan struct{})
	close(cancel)
	before := len(srv.spadeEvents())
	if tw.SimulateClipPlayback(streamer, entities.Clip{ID: "c1", DurationSeconds: 60}, time.Hour, cancel, always) {
		t.Fatalf("cancelled watch should report failure")
	}
	if got := len(srv.spadeEvents()) - before; got != 1 {
		t.Fatalf("cancelled watch should only send video-play, sent %d events", got)
	}
}

func TestSimulateVODPlayback(t *testing.T) {
	shortenWeeklyIntervals(t)
	srv := &weeklyTestServer{}
	tw := newWeeklyTestTwitch(t, srv)
	streamer := weeklyTestStreamer()
	vod := entities.VOD{ID: "v1", LengthSeconds: 7200}

	var sent atomic.Int32
	stillMissing := func() bool {
		n := int32(len(srv.spadeEvents()))
		sent.Store(n)
		return n < 3
	}
	if !tw.SimulateVODPlayback(streamer, vod, time.Second, make(chan struct{}), stillMissing) {
		t.Fatalf("expected success once progress is made")
	}
	events := srv.spadeEvents()
	if len(events) != 3 {
		t.Fatalf("expected 3 minute-watched events, got %d", len(events))
	}
	props := events[0].Properties
	if events[0].Event != "minute-watched" || props["vod_id"] != "v1" || props["content_mode"] != "vod" || props["live"] != false || props["channel"] != "streamer" || props["broadcast_id"] != nil {
		t.Fatalf("unexpected minute-watched event: %+v", events[0])
	}

	cancel := make(chan struct{})
	close(cancel)
	if tw.SimulateVODPlayback(streamer, vod, time.Hour, cancel, func() bool { return true }) {
		t.Fatalf("cancelled VOD watch should report failure")
	}
}

func TestParseWeeklyRewardsNotification(t *testing.T) {
	flat := `{"viewerId":"viewer-1","channelId":"123456","eventId":"event-1","daysVisitedThisWeek":2,"accumulatedWeeks":null,
"notificationType":"DAY_COMPLETED","currentReward":{"tier":1,"channelPoints":200,"badgeSetId":"weekly","badgeVersion":"1"},
"eventConfig":{"daysRequiredPerWeek":3}}`
	n, err := ParseWeeklyRewardsNotification([]byte(flat))
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if n.ChannelID != "123456" || n.DaysVisitedThisWeek != 2 || n.AccumulatedWeeks != nil || n.NotificationType != "DAY_COMPLETED" {
		t.Fatalf("unexpected notification: %+v", n)
	}
	if n.CurrentReward.ChannelPoints != 200 || n.CurrentReward.Badge.SetID != "weekly" || n.DaysRequiredPerWeek != 3 {
		t.Fatalf("unexpected reward/config: %+v", n)
	}

	wrapped := `{"type":"weekly-reward","data":{"channelId":"123456","notificationType":"WEEK_COMPLETED","accumulatedWeeks":1}}`
	n, err = ParseWeeklyRewardsNotification([]byte(wrapped))
	if err != nil || n.NotificationType != entities.WeeklyRewardsNotificationWeekCompleted || n.AccumulatedWeeks == nil || *n.AccumulatedWeeks != 1 {
		t.Fatalf("unexpected wrapped notification: %+v %v", n, err)
	}

	if _, err := ParseWeeklyRewardsNotification([]byte(`{"channelId":"1"}`)); err == nil {
		t.Fatalf("a message without notificationType should be rejected")
	}
	if _, err := ParseWeeklyRewardsNotification([]byte(`not json`)); err == nil {
		t.Fatalf("invalid json should be rejected")
	}
}

func TestPubSubWeeklyRewardsTopicAndRouting(t *testing.T) {
	enabled := weeklyTestStreamer()
	disabled := newTestStreamer(false)
	disabled.ChannelID = "999"
	tw := &Twitch{twitchLogin: &TwitchLogin{userID: "viewer-1"}}
	client := NewPubSubClient(tw, &stubPubSubLogger{}, nil, []*entities.Streamer{enabled, disabled}, false, nil, nil)

	topics, err := client.buildTopics()
	if err != nil {
		t.Fatalf("buildTopics error: %v", err)
	}
	var weekly []string
	for _, topic := range topics {
		if strings.HasPrefix(topic, "weekly-rewards.") {
			weekly = append(weekly, topic)
		}
	}
	if len(weekly) != 1 || weekly[0] != "weekly-rewards.viewer-1.123456" {
		t.Fatalf("unexpected weekly-rewards topics: %v", weekly)
	}

	var got []entities.WeeklyRewardsNotification
	var gotStreamer *entities.Streamer
	client.SetWeeklyRewardsHandler(func(s *entities.Streamer, n entities.WeeklyRewardsNotification) {
		gotStreamer = s
		got = append(got, n)
	})
	message, _ := json.Marshal(`{"notificationType":"DAY_COMPLETED","daysVisitedThisWeek":1}`)
	envelope := `{"type":"MESSAGE","data":{"topic":"weekly-rewards.viewer-1.123456","message":` + string(message) + `}}`
	if err := client.handleMessage([]byte(envelope), nil); err != nil {
		t.Fatalf("handleMessage error: %v", err)
	}
	if len(got) != 1 || gotStreamer != enabled || got[0].DaysVisitedThisWeek != 1 {
		t.Fatalf("notification not routed to the streamer from the topic: %+v", got)
	}

	unknown := `{"type":"MESSAGE","data":{"topic":"weekly-rewards.viewer-1.555","message":` + string(message) + `}}`
	if err := client.handleMessage([]byte(unknown), nil); err != nil || len(got) != 1 {
		t.Fatalf("notification for an unknown channel should be ignored, err=%v calls=%d", err, len(got))
	}
}
