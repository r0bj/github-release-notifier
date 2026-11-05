package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestFormatSlackMessageIncludesReleaseNotes(t *testing.T) {
	repo := RepoConfig{
		owner: "prometheus",
		name:  "prometheus",
	}
	release := GitHubRelease{
		TagName: "v1.2.3",
		Name:    "Release v1.2.3",
		Body:    "## What's Changed\n\n* Fix startup race\n* Improve metrics",
		HTMLURL: "https://github.com/prometheus/prometheus/releases/tag/v1.2.3",
	}

	message := formatSlackMessage(repo, release, false)

	assertContains(t, message, "*New release* for *prometheus/prometheus*")
	assertContains(t, message, "*<https://github.com/prometheus/prometheus/releases/tag/v1.2.3|Release v1.2.3>* (`v1.2.3`)")
	assertContains(t, message, "*Release notes:*")
	assertContains(t, message, "> ## What's Changed")
	assertContains(t, message, "> * Fix startup race")
	assertContains(t, message, "https://github.com/prometheus/prometheus/releases/tag/v1.2.3")
}

func TestFormatSlackMessageOmitsEmptyReleaseNotes(t *testing.T) {
	repo := RepoConfig{
		owner: "immich-app",
		name:  "immich",
	}
	release := GitHubRelease{
		TagName: "v1.0.0",
		HTMLURL: "https://github.com/immich-app/immich/releases/tag/v1.0.0",
	}

	message := formatSlackMessage(repo, release, false)

	if strings.Contains(message, "Release notes") {
		t.Fatalf("expected release notes section to be omitted, got: %q", message)
	}

	assertContains(t, message, "*<https://github.com/immich-app/immich/releases/tag/v1.0.0|v1.0.0>* (`v1.0.0`)")
}

func TestFormatSlackMessageMarksUpdates(t *testing.T) {
	repo := RepoConfig{
		owner: "prometheus",
		name:  "prometheus",
	}
	release := GitHubRelease{
		TagName: "v1.2.3",
		HTMLURL: "https://github.com/prometheus/prometheus/releases/tag/v1.2.3",
	}

	message := formatSlackMessage(repo, release, true)

	assertContains(t, message, "*Updated release* for *prometheus/prometheus*")
}

func TestFormatReleaseNotesTruncatesLongBody(t *testing.T) {
	notes := formatReleaseNotes(strings.Repeat("a", maxReleaseNotesRunes+10), maxReleaseNotesRunes)

	if got := len([]rune(notes)); got != maxReleaseNotesRunes+len([]rune(releaseNotesTruncation)) {
		t.Fatalf("unexpected truncated length: got %d", got)
	}

	if !strings.HasSuffix(notes, releaseNotesTruncation) {
		t.Fatalf("expected truncation suffix, got: %q", notes)
	}
}

func TestNormalizeRepoConfigsParsesURL(t *testing.T) {
	repos := []RepoConfig{
		{URL: "https://github.com/prometheus/prometheus"},
		{URL: "https://github.com/immich-app/immich", ExcludePrereleases: boolPtr(true), ExcludeUpdates: boolPtr(true)},
	}

	if err := normalizeRepoConfigs(repos); err != nil {
		t.Fatalf("normalizeRepoConfigs returned error: %v", err)
	}

	if repos[0].owner != "prometheus" || repos[0].name != "prometheus" {
		t.Fatalf("unexpected repo[0]: owner=%q name=%q", repos[0].owner, repos[0].name)
	}

	if repos[1].owner != "immich-app" || repos[1].name != "immich" {
		t.Fatalf("unexpected repo[1]: owner=%q name=%q", repos[1].owner, repos[1].name)
	}

	if repos[1].ExcludePrereleases == nil || !*repos[1].ExcludePrereleases {
		t.Fatal("expected exclude_prereleases to be preserved")
	}

	if repos[1].ExcludeUpdates == nil || !*repos[1].ExcludeUpdates {
		t.Fatal("expected exclude_updates to be preserved")
	}
}

func TestNormalizeRepoConfigsLogsDuplicateRepos(t *testing.T) {
	var logBuffer bytes.Buffer
	oldOutput := log.Writer()
	log.SetOutput(&logBuffer)
	defer log.SetOutput(oldOutput)

	repos := []RepoConfig{
		{URL: "https://github.com/prometheus/prometheus"},
		{URL: "https://github.com/prometheus/prometheus/"},
	}

	if err := normalizeRepoConfigs(repos); err != nil {
		t.Fatalf("normalizeRepoConfigs returned error: %v", err)
	}

	assertContains(t, logBuffer.String(), "duplicate repo config: repos[1] duplicates repos[0] for prometheus/prometheus")
}

func TestLoadConfigAppliesRuntimeDefaults(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	err := os.WriteFile(configPath, []byte(`repos:
- url: https://github.com/prometheus/prometheus
`), 0644)
	if err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	cfg, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("loadConfig returned error: %v", err)
	}

	if cfg.CheckInterval != defaultCheckInterval {
		t.Fatalf("expected default check interval %s, got %s", defaultCheckInterval, cfg.CheckInterval)
	}

	if cfg.StateFile != defaultStateFile {
		t.Fatalf("expected default state file %q, got %q", defaultStateFile, cfg.StateFile)
	}

	if cfg.MaxConcurrentChecks != defaultMaxConcurrentChecks {
		t.Fatalf("expected default max concurrent checks %d, got %d", defaultMaxConcurrentChecks, cfg.MaxConcurrentChecks)
	}
}

func TestLoadConfigPreservesExplicitFalseDefaults(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	err := os.WriteFile(configPath, []byte(`defaults:
  exclude_prereleases: false
  exclude_updates: false
  notify_on_first_run: false
repos:
- url: https://github.com/prometheus/prometheus
`), 0644)
	if err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	cfg, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("loadConfig returned error: %v", err)
	}

	if cfg.Defaults.ExcludePrereleases == nil || *cfg.Defaults.ExcludePrereleases {
		t.Fatal("expected explicit exclude_prereleases=false to be preserved")
	}

	if cfg.Defaults.ExcludeUpdates == nil || *cfg.Defaults.ExcludeUpdates {
		t.Fatal("expected explicit exclude_updates=false to be preserved")
	}

	if cfg.Defaults.NotifyOnFirstRun == nil || *cfg.Defaults.NotifyOnFirstRun {
		t.Fatal("expected explicit notify_on_first_run=false to be preserved")
	}
}

func TestPruneStateRemovesReposMissingFromConfig(t *testing.T) {
	state := &State{
		Repos: map[string]RepoState{
			"prometheus/prometheus": {},
			"immich-app/immich":     {},
			"grafana/grafana":       {},
		},
	}
	repos := []RepoConfig{
		{owner: "prometheus", name: "prometheus"},
		{owner: "immich-app", name: "immich"},
	}

	removed := pruneState(state, repos)

	if removed != 1 {
		t.Fatalf("expected 1 removed repo, got %d", removed)
	}

	if _, ok := state.Repos["grafana/grafana"]; ok {
		t.Fatal("expected stale repo to be removed")
	}

	if _, ok := state.Repos["prometheus/prometheus"]; !ok {
		t.Fatal("expected configured repo to stay in state")
	}

	if _, ok := state.Repos["immich-app/immich"]; !ok {
		t.Fatal("expected configured repo to stay in state")
	}
}

func TestMarkSeenPreservesFirstSeenAtOnReleaseUpdate(t *testing.T) {
	firstSeenAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	oldUpdatedAt := time.Date(2026, 1, 3, 3, 4, 5, 0, time.UTC)
	newUpdatedAt := time.Date(2026, 1, 4, 3, 4, 5, 0, time.UTC)
	repoState := RepoState{
		SeenReleaseIDs: map[int64]SeenRelease{
			123: {
				TagName:     "v1.2.3",
				FirstSeenAt: firstSeenAt,
				UpdatedAt:   oldUpdatedAt,
			},
		},
	}

	markSeen(repoState, GitHubRelease{
		ID:        123,
		TagName:   "v1.2.3",
		UpdatedAt: newUpdatedAt,
	})

	seenRelease := repoState.SeenReleaseIDs[123]
	if !seenRelease.FirstSeenAt.Equal(firstSeenAt) {
		t.Fatalf("expected FirstSeenAt to be preserved, got %s", seenRelease.FirstSeenAt)
	}

	if !seenRelease.UpdatedAt.Equal(newUpdatedAt) {
		t.Fatalf("expected UpdatedAt to be refreshed, got %s", seenRelease.UpdatedAt)
	}
}

func TestCheckRepoNotifiesReleaseUpdateWhenUpdatesAreIncluded(t *testing.T) {
	oldUpdatedAt := time.Date(2026, 1, 3, 3, 4, 5, 0, time.UTC)
	newUpdatedAt := time.Date(2026, 1, 4, 3, 4, 5, 0, time.UTC)
	release := GitHubRelease{
		ID:        123,
		TagName:   "v1.2.3",
		Name:      "Release v1.2.3",
		HTMLURL:   "https://github.com/prometheus/prometheus/releases/tag/v1.2.3",
		CreatedAt: oldUpdatedAt,
		UpdatedAt: newUpdatedAt,
	}

	var slackMessages []SlackMessage
	client := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			switch r.URL.Host {
			case "github.test":
				if r.URL.Path != "/repos/prometheus/prometheus/releases" {
					t.Fatalf("unexpected GitHub path: %s", r.URL.Path)
				}

				return jsonResponse(t, []GitHubRelease{release}), nil
			case "slack.test":
				var message SlackMessage
				if err := json.NewDecoder(r.Body).Decode(&message); err != nil {
					t.Fatalf("failed to decode slack payload: %v", err)
				}
				slackMessages = append(slackMessages, message)
				return emptyResponse(http.StatusOK), nil
			default:
				t.Fatalf("unexpected host: %s", r.URL.Host)
				return nil, nil
			}
		}),
	}

	oldGitHubAPIBaseURL := githubAPIBaseURL
	githubAPIBaseURL = "https://github.test"
	defer func() {
		githubAPIBaseURL = oldGitHubAPIBaseURL
	}()

	excludeUpdates := false
	cfg := &Config{}
	repo := RepoConfig{
		owner:          "prometheus",
		name:           "prometheus",
		ExcludeUpdates: &excludeUpdates,
	}
	repoState := RepoState{
		Initialized: true,
		SeenReleaseIDs: map[int64]SeenRelease{
			123: {
				TagName:     "v1.2.3",
				FirstSeenAt: oldUpdatedAt,
				UpdatedAt:   oldUpdatedAt,
			},
		},
	}

	repoState, err := checkRepo(context.Background(), client, cfg, repo, repoState, "", SlackConfig{WebhookURL: "https://slack.test/webhook"})
	if err != nil {
		t.Fatalf("checkRepo returned error: %v", err)
	}

	if len(slackMessages) != 1 {
		t.Fatalf("expected 1 slack message, got %d", len(slackMessages))
	}

	assertContains(t, slackMessages[0].Text, "*Updated release*")

	seenRelease := repoState.SeenReleaseIDs[123]
	if !seenRelease.UpdatedAt.Equal(newUpdatedAt) {
		t.Fatalf("expected state UpdatedAt to be refreshed, got %s", seenRelease.UpdatedAt)
	}
}

func TestCheckRepoSkipsReleaseUpdateWhenUpdatesAreExcluded(t *testing.T) {
	oldUpdatedAt := time.Date(2026, 1, 3, 3, 4, 5, 0, time.UTC)
	newUpdatedAt := time.Date(2026, 1, 4, 3, 4, 5, 0, time.UTC)
	release := GitHubRelease{
		ID:        123,
		TagName:   "v1.2.3",
		HTMLURL:   "https://github.com/prometheus/prometheus/releases/tag/v1.2.3",
		CreatedAt: oldUpdatedAt,
		UpdatedAt: newUpdatedAt,
	}

	client := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			switch r.URL.Host {
			case "github.test":
				return jsonResponse(t, []GitHubRelease{release}), nil
			case "slack.test":
				t.Fatal("did not expect slack notification")
				return nil, nil
			default:
				t.Fatalf("unexpected host: %s", r.URL.Host)
				return nil, nil
			}
		}),
	}

	oldGitHubAPIBaseURL := githubAPIBaseURL
	githubAPIBaseURL = "https://github.test"
	defer func() {
		githubAPIBaseURL = oldGitHubAPIBaseURL
	}()

	cfg := &Config{}
	cfg.Defaults.ExcludeUpdates = boolPtr(true)
	repo := RepoConfig{
		owner: "prometheus",
		name:  "prometheus",
	}
	repoState := RepoState{
		Initialized: true,
		SeenReleaseIDs: map[int64]SeenRelease{
			123: {
				TagName:     "v1.2.3",
				FirstSeenAt: oldUpdatedAt,
				UpdatedAt:   oldUpdatedAt,
			},
		},
	}

	repoState, err := checkRepo(context.Background(), client, cfg, repo, repoState, "", SlackConfig{WebhookURL: "https://slack.test/webhook"})
	if err != nil {
		t.Fatalf("checkRepo returned error: %v", err)
	}

	seenRelease := repoState.SeenReleaseIDs[123]
	if !seenRelease.UpdatedAt.Equal(newUpdatedAt) {
		t.Fatalf("expected state UpdatedAt to be refreshed, got %s", seenRelease.UpdatedAt)
	}
}

func TestFetchGitHubReleasesIncludesForbiddenDetails(t *testing.T) {
	client := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Status:     "403 Forbidden",
				Body: io.NopCloser(strings.NewReader(`{
  "message": "API rate limit exceeded for 198.51.100.1.",
  "documentation_url": "https://docs.github.com/rest/overview/resources-in-the-rest-api#rate-limiting"
}`)),
				Header: http.Header{
					"X-RateLimit-Remaining": []string{"0"},
					"X-RateLimit-Reset":     []string{"1770000000"},
					"X-RateLimit-Resource":  []string{"core"},
				},
			}, nil
		}),
	}
	restoreGitHubAPIBaseURL := useTestGitHubAPIBaseURL()
	defer restoreGitHubAPIBaseURL()

	_, err := fetchGitHubReleases(context.Background(), client, "", "prometheus", "prometheus")
	if err == nil {
		t.Fatal("expected error")
	}

	assertContains(t, err.Error(), "github api returned status 403 Forbidden for prometheus/prometheus")
	assertContains(t, err.Error(), `message="API rate limit exceeded for 198.51.100.1."`)
	assertContains(t, err.Error(), "rate_limit_remaining=0")
	assertContains(t, err.Error(), "rate_limit_resource=core")
}

func TestRunOnceLimitsConcurrentRepoChecks(t *testing.T) {
	var activeRequests int64
	var maxActiveRequests int64

	client := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			switch r.URL.Host {
			case "github.test":
				active := atomic.AddInt64(&activeRequests, 1)
				for {
					maxActive := atomic.LoadInt64(&maxActiveRequests)
					if active <= maxActive || atomic.CompareAndSwapInt64(&maxActiveRequests, maxActive, active) {
						break
					}
				}

				time.Sleep(20 * time.Millisecond)
				atomic.AddInt64(&activeRequests, -1)
				return jsonResponse(t, []GitHubRelease{}), nil
			case "slack.test":
				t.Fatal("did not expect slack notification")
				return nil, nil
			default:
				t.Fatalf("unexpected host: %s", r.URL.Host)
				return nil, nil
			}
		}),
	}
	restoreGitHubAPIBaseURL := useTestGitHubAPIBaseURL()
	defer restoreGitHubAPIBaseURL()

	cfg := &Config{
		MaxConcurrentChecks: 3,
		Repos: []RepoConfig{
			{owner: "owner", name: "repo-1"},
			{owner: "owner", name: "repo-2"},
			{owner: "owner", name: "repo-3"},
			{owner: "owner", name: "repo-4"},
			{owner: "owner", name: "repo-5"},
			{owner: "owner", name: "repo-6"},
			{owner: "owner", name: "repo-7"},
			{owner: "owner", name: "repo-8"},
		},
	}
	state := &State{Repos: map[string]RepoState{}}

	runOnce(context.Background(), client, cfg, state, "", SlackConfig{WebhookURL: "https://slack.test/webhook"})

	if maxActiveRequests > int64(cfg.MaxConcurrentChecks) {
		t.Fatalf("expected at most %d concurrent requests, got %d", cfg.MaxConcurrentChecks, maxActiveRequests)
	}

	if len(state.Repos) != len(cfg.Repos) {
		t.Fatalf("expected %d state repos, got %d", len(cfg.Repos), len(state.Repos))
	}
}

func TestCheckRepoSkipsPrereleasesByDefault(t *testing.T) {
	updatedAt := time.Date(2026, 1, 4, 3, 4, 5, 0, time.UTC)
	release := GitHubRelease{
		ID:          123,
		TagName:     "v1.2.3-rc.1",
		Prerelease:  true,
		HTMLURL:     "https://github.com/prometheus/prometheus/releases/tag/v1.2.3-rc.1",
		CreatedAt:   updatedAt,
		UpdatedAt:   updatedAt,
		PublishedAt: &updatedAt,
	}

	client := testClientForReleases(t, []GitHubRelease{release}, nil, false)
	restoreGitHubAPIBaseURL := useTestGitHubAPIBaseURL()
	defer restoreGitHubAPIBaseURL()

	cfg := &Config{}
	repo := RepoConfig{
		owner: "prometheus",
		name:  "prometheus",
	}
	repoState := RepoState{}

	repoState, err := checkRepo(context.Background(), client, cfg, repo, repoState, "", SlackConfig{WebhookURL: "https://slack.test/webhook"})
	if err != nil {
		t.Fatalf("checkRepo returned error: %v", err)
	}

	if len(repoState.SeenReleaseIDs) != 0 {
		t.Fatal("expected prerelease to stay unseen when prereleases are excluded")
	}
}

func TestCheckRepoAllowsPrereleaseWhenRepoOverrideDisablesExclusion(t *testing.T) {
	updatedAt := time.Date(2026, 1, 4, 3, 4, 5, 0, time.UTC)
	release := GitHubRelease{
		ID:          123,
		TagName:     "v1.2.3-rc.1",
		Prerelease:  true,
		HTMLURL:     "https://github.com/prometheus/prometheus/releases/tag/v1.2.3-rc.1",
		CreatedAt:   updatedAt,
		UpdatedAt:   updatedAt,
		PublishedAt: &updatedAt,
	}

	var slackMessages []SlackMessage
	client := testClientForReleases(t, []GitHubRelease{release}, &slackMessages, true)
	restoreGitHubAPIBaseURL := useTestGitHubAPIBaseURL()
	defer restoreGitHubAPIBaseURL()

	includePrereleases := false
	notifyOnFirstRun := true
	cfg := &Config{}
	repo := RepoConfig{
		owner:              "prometheus",
		name:               "prometheus",
		ExcludePrereleases: &includePrereleases,
		NotifyOnFirstRun:   &notifyOnFirstRun,
	}
	repoState := RepoState{}

	repoState, err := checkRepo(context.Background(), client, cfg, repo, repoState, "", SlackConfig{WebhookURL: "https://slack.test/webhook"})
	if err != nil {
		t.Fatalf("checkRepo returned error: %v", err)
	}

	if len(slackMessages) != 1 {
		t.Fatalf("expected 1 slack message, got %d", len(slackMessages))
	}
}

func TestCheckRepoUsesGlobalDefaults(t *testing.T) {
	updatedAt := time.Date(2026, 1, 4, 3, 4, 5, 0, time.UTC)
	release := GitHubRelease{
		ID:          123,
		TagName:     "v1.2.3-rc.1",
		Prerelease:  true,
		HTMLURL:     "https://github.com/prometheus/prometheus/releases/tag/v1.2.3-rc.1",
		CreatedAt:   updatedAt,
		UpdatedAt:   updatedAt,
		PublishedAt: &updatedAt,
	}

	var slackMessages []SlackMessage
	client := testClientForReleases(t, []GitHubRelease{release}, &slackMessages, true)
	restoreGitHubAPIBaseURL := useTestGitHubAPIBaseURL()
	defer restoreGitHubAPIBaseURL()

	cfg := &Config{}
	cfg.Defaults.ExcludePrereleases = boolPtr(false)
	cfg.Defaults.NotifyOnFirstRun = boolPtr(true)
	repo := RepoConfig{
		owner: "prometheus",
		name:  "prometheus",
	}
	repoState := RepoState{}

	repoState, err := checkRepo(context.Background(), client, cfg, repo, repoState, "", SlackConfig{WebhookURL: "https://slack.test/webhook"})
	if err != nil {
		t.Fatalf("checkRepo returned error: %v", err)
	}

	if len(slackMessages) != 1 {
		t.Fatalf("expected 1 slack message, got %d", len(slackMessages))
	}
}

func TestCheckRepoDoesNotNotifyOnFirstRunByDefault(t *testing.T) {
	updatedAt := time.Date(2026, 1, 4, 3, 4, 5, 0, time.UTC)
	release := GitHubRelease{
		ID:        123,
		TagName:   "v1.2.3",
		HTMLURL:   "https://github.com/prometheus/prometheus/releases/tag/v1.2.3",
		CreatedAt: updatedAt,
		UpdatedAt: updatedAt,
	}

	client := testClientForReleases(t, []GitHubRelease{release}, nil, false)
	restoreGitHubAPIBaseURL := useTestGitHubAPIBaseURL()
	defer restoreGitHubAPIBaseURL()

	cfg := &Config{}
	repo := RepoConfig{
		owner: "prometheus",
		name:  "prometheus",
	}
	repoState := RepoState{}

	repoState, err := checkRepo(context.Background(), client, cfg, repo, repoState, "", SlackConfig{WebhookURL: "https://slack.test/webhook"})
	if err != nil {
		t.Fatalf("checkRepo returned error: %v", err)
	}

	if _, ok := repoState.SeenReleaseIDs[123]; !ok {
		t.Fatal("expected release to be marked seen on first run")
	}
}

func TestCheckRepoNotifiesOnFirstRunWhenRepoOverrideEnablesIt(t *testing.T) {
	updatedAt := time.Date(2026, 1, 4, 3, 4, 5, 0, time.UTC)
	release := GitHubRelease{
		ID:        123,
		TagName:   "v1.2.3",
		HTMLURL:   "https://github.com/prometheus/prometheus/releases/tag/v1.2.3",
		CreatedAt: updatedAt,
		UpdatedAt: updatedAt,
	}

	var slackMessages []SlackMessage
	client := testClientForReleases(t, []GitHubRelease{release}, &slackMessages, true)
	restoreGitHubAPIBaseURL := useTestGitHubAPIBaseURL()
	defer restoreGitHubAPIBaseURL()

	notifyOnFirstRun := true
	cfg := &Config{}
	repo := RepoConfig{
		owner:            "prometheus",
		name:             "prometheus",
		NotifyOnFirstRun: &notifyOnFirstRun,
	}
	repoState := RepoState{}

	repoState, err := checkRepo(context.Background(), client, cfg, repo, repoState, "", SlackConfig{WebhookURL: "https://slack.test/webhook"})
	if err != nil {
		t.Fatalf("checkRepo returned error: %v", err)
	}

	if len(slackMessages) != 1 {
		t.Fatalf("expected 1 slack message, got %d", len(slackMessages))
	}
}

func TestParseGitHubRepoURL(t *testing.T) {
	tests := []struct {
		name      string
		rawURL    string
		wantOwner string
		wantName  string
		wantErr   bool
	}{
		{
			name:      "standard URL",
			rawURL:    "https://github.com/prometheus/prometheus",
			wantOwner: "prometheus",
			wantName:  "prometheus",
		},
		{
			name:      "trailing slash",
			rawURL:    "https://github.com/immich-app/immich/",
			wantOwner: "immich-app",
			wantName:  "immich",
		},
		{
			name:      "git suffix",
			rawURL:    "https://github.com/grafana/grafana.git",
			wantOwner: "grafana",
			wantName:  "grafana",
		},
		{
			name:    "not GitHub",
			rawURL:  "https://gitlab.com/prometheus/prometheus",
			wantErr: true,
		},
		{
			name:    "missing repo",
			rawURL:  "https://github.com/prometheus",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			owner, name, err := parseGitHubRepoURL(tt.rawURL)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}

			if err != nil {
				t.Fatalf("parseGitHubRepoURL returned error: %v", err)
			}

			if owner != tt.wantOwner || name != tt.wantName {
				t.Fatalf("unexpected result: owner=%q name=%q", owner, name)
			}
		})
	}
}

func boolPtr(value bool) *bool {
	return &value
}

func assertContains(t *testing.T, value string, substring string) {
	t.Helper()

	if !strings.Contains(value, substring) {
		t.Fatalf("expected %q to contain %q", value, substring)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return fn(r)
}

func jsonResponse(t *testing.T, value any) *http.Response {
	t.Helper()

	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("failed to marshal test response: %v", err)
	}

	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Body:       io.NopCloser(strings.NewReader(string(data))),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}
}

func emptyResponse(statusCode int) *http.Response {
	return &http.Response{
		StatusCode: statusCode,
		Status:     http.StatusText(statusCode),
		Body:       io.NopCloser(strings.NewReader("")),
		Header:     http.Header{},
	}
}

func testClientForReleases(
	t *testing.T,
	releases []GitHubRelease,
	slackMessages *[]SlackMessage,
	expectSlack bool,
) *http.Client {
	t.Helper()

	return &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			switch r.URL.Host {
			case "github.test":
				return jsonResponse(t, releases), nil
			case "slack.test":
				if !expectSlack {
					t.Fatal("did not expect slack notification")
				}

				var message SlackMessage
				if err := json.NewDecoder(r.Body).Decode(&message); err != nil {
					t.Fatalf("failed to decode slack payload: %v", err)
				}
				*slackMessages = append(*slackMessages, message)
				return emptyResponse(http.StatusOK), nil
			default:
				t.Fatalf("unexpected host: %s", r.URL.Host)
				return nil, nil
			}
		}),
	}
}

func useTestGitHubAPIBaseURL() func() {
	oldGitHubAPIBaseURL := githubAPIBaseURL
	githubAPIBaseURL = "https://github.test"

	return func() {
		githubAPIBaseURL = oldGitHubAPIBaseURL
	}
}

func useTestSlackAPIBaseURL() func() {
	oldSlackAPIBaseURL := slackAPIBaseURL
	slackAPIBaseURL = "https://slackapi.test/api"

	return func() {
		slackAPIBaseURL = oldSlackAPIBaseURL
	}
}

func TestSendSlackMessageUsesBotToken(t *testing.T) {
	defer useTestSlackAPIBaseURL()()

	var got SlackMessage
	var authHeader string
	client := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Host != "slackapi.test" || r.URL.Path != "/api/chat.postMessage" {
				t.Fatalf("unexpected request: %s", r.URL)
			}
			authHeader = r.Header.Get("Authorization")
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Fatalf("failed to decode slack payload: %v", err)
			}
			return jsonResponse(t, map[string]any{"ok": true}), nil
		}),
	}

	slackCfg := SlackConfig{
		WebhookURL: "https://slack.test/webhook",
		BotToken:   "xoxb-test",
		Channel:    "#releases",
	}
	if err := sendSlackMessage(context.Background(), client, slackCfg, "hello"); err != nil {
		t.Fatalf("sendSlackMessage returned error: %v", err)
	}

	if authHeader != "Bearer xoxb-test" {
		t.Fatalf("unexpected Authorization header: %q", authHeader)
	}
	if got.Channel != "#releases" || got.Text != "hello" {
		t.Fatalf("unexpected payload: %+v", got)
	}
}

func TestSendSlackMessageReturnsSlackAPIError(t *testing.T) {
	defer useTestSlackAPIBaseURL()()

	client := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return jsonResponse(t, map[string]any{"ok": false, "error": "not_in_channel"}), nil
		}),
	}

	slackCfg := SlackConfig{BotToken: "xoxb-test", Channel: "C123"}
	err := sendSlackMessage(context.Background(), client, slackCfg, "hello")
	if err == nil {
		t.Fatal("expected error")
	}
	assertContains(t, err.Error(), "not_in_channel")
}

func TestSlackConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     SlackConfig
		wantErr bool
	}{
		{name: "webhook", cfg: SlackConfig{WebhookURL: "https://slack.test/webhook"}},
		{name: "bot token", cfg: SlackConfig{BotToken: "xoxb-test", Channel: "C123"}},
		{name: "bot token without channel", cfg: SlackConfig{BotToken: "xoxb-test"}, wantErr: true},
		{name: "bot token without channel even with webhook", cfg: SlackConfig{BotToken: "xoxb-test", WebhookURL: "https://slack.test/webhook"}, wantErr: true},
		{name: "empty", cfg: SlackConfig{}, wantErr: true},
		{name: "channel only", cfg: SlackConfig{Channel: "C123"}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
