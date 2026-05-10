package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	slackWebhookURLEnv         = "SLACK_WEBHOOK_URL"
	githubTokenEnv             = "GITHUB_TOKEN"
	defaultGitHubAPIBaseURL    = "https://api.github.com"
	defaultCheckInterval       = 30 * time.Minute
	defaultMaxConcurrentChecks = 5
	defaultStateFile           = "/data/state.json"
	defaultExcludePrerelease   = true
	defaultExcludeUpdates      = true
	defaultNotifyOnFirstRun    = false
	maxReleaseNotesRunes       = 1200
	releaseNotesTruncation     = "\n..."
	maxBlankReleaseNoteLines   = 1
	maxGitHubErrorBodyBytes    = 4096
)

var githubAPIBaseURL = defaultGitHubAPIBaseURL

type Config struct {
	CheckInterval       time.Duration `yaml:"check_interval"`
	MaxConcurrentChecks int           `yaml:"max_concurrent_checks"`
	StateFile           string        `yaml:"state_file"`

	Defaults DefaultConfig `yaml:"defaults"`

	Repos []RepoConfig `yaml:"repos"`
}

type DefaultConfig struct {
	ExcludePrereleases *bool `yaml:"exclude_prereleases"`
	ExcludeUpdates     *bool `yaml:"exclude_updates"`
	NotifyOnFirstRun   *bool `yaml:"notify_on_first_run"`
}

type RepoConfig struct {
	URL string `yaml:"url"`

	owner string
	name  string

	ExcludePrereleases *bool `yaml:"exclude_prereleases"`
	ExcludeUpdates     *bool `yaml:"exclude_updates"`
	NotifyOnFirstRun   *bool `yaml:"notify_on_first_run"`
}

type GitHubRelease struct {
	ID          int64      `json:"id"`
	TagName     string     `json:"tag_name"`
	Name        string     `json:"name"`
	Body        string     `json:"body"`
	Draft       bool       `json:"draft"`
	Prerelease  bool       `json:"prerelease"`
	HTMLURL     string     `json:"html_url"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	PublishedAt *time.Time `json:"published_at"`
}

type State struct {
	Repos map[string]RepoState `json:"repos"`
}

type RepoState struct {
	Initialized    bool                  `json:"initialized"`
	SeenReleaseIDs map[int64]SeenRelease `json:"seen_release_ids"`
}

type SeenRelease struct {
	TagName     string    `json:"tag_name"`
	FirstSeenAt time.Time `json:"first_seen_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type SlackMessage struct {
	Text string `json:"text"`
}

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	configFile := getenvDefault("CONFIG_FILE", "/config/config.yaml")

	cfg, err := loadConfig(configFile)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	if cfg.CheckInterval == 0 {
		cfg.CheckInterval = defaultCheckInterval
	}

	if cfg.MaxConcurrentChecks <= 0 {
		cfg.MaxConcurrentChecks = defaultMaxConcurrentChecks
	}

	if cfg.StateFile == "" {
		cfg.StateFile = defaultStateFile
	}

	slackWebhookURL := os.Getenv(slackWebhookURLEnv)
	if slackWebhookURL == "" {
		log.Fatalf("%s is required", slackWebhookURLEnv)
	}

	githubToken := os.Getenv(githubTokenEnv)

	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	state, err := loadState(cfg.StateFile)
	if err != nil {
		log.Fatalf("failed to load state: %v", err)
	}

	if removed := pruneState(state, cfg.Repos); removed > 0 {
		log.Printf("removed %d stale repo(s) from state", removed)
	}

	log.Printf(
		"github-release-notifier started, repos=%d, interval=%s, max_concurrent_checks=%d",
		len(cfg.Repos),
		cfg.CheckInterval,
		cfg.MaxConcurrentChecks,
	)

	runOnce(ctx, client, cfg, state, githubToken, slackWebhookURL)

	if err := saveState(cfg.StateFile, state); err != nil {
		log.Printf("failed to save state: %v", err)
	}

	ticker := time.NewTicker(cfg.CheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("shutdown requested")
			return

		case <-ticker.C:
			jitter := time.Duration(rand.Intn(120)) * time.Second
			log.Printf("waiting jitter=%s before check", jitter)

			select {
			case <-ctx.Done():
				log.Println("shutdown requested")
				return
			case <-time.After(jitter):
			}

			runOnce(ctx, client, cfg, state, githubToken, slackWebhookURL)

			if err := saveState(cfg.StateFile, state); err != nil {
				log.Printf("failed to save state: %v", err)
			}
		}
	}
}

func runOnce(
	ctx context.Context,
	client *http.Client,
	cfg *Config,
	state *State,
	githubToken string,
	slackWebhookURL string,
) {
	var wg sync.WaitGroup
	var stateMu sync.Mutex
	maxConcurrentChecks := cfg.MaxConcurrentChecks
	if maxConcurrentChecks <= 0 {
		maxConcurrentChecks = defaultMaxConcurrentChecks
	}
	semaphore := make(chan struct{}, maxConcurrentChecks)

	for _, repo := range cfg.Repos {
		if repo.owner == "" || repo.name == "" {
			log.Printf("skipping invalid repo config: url=%q", repo.URL)
			continue
		}

		wg.Add(1)
		go func(repo RepoConfig) {
			defer wg.Done()

			select {
			case semaphore <- struct{}{}:
				defer func() {
					<-semaphore
				}()
			case <-ctx.Done():
				return
			}

			repoState := repoStateSnapshot(state, &stateMu, repo)

			updatedRepoState, err := checkRepo(ctx, client, cfg, repo, repoState, githubToken, slackWebhookURL)
			if err != nil {
				log.Printf("repo %s/%s failed: %v", repo.owner, repo.name, err)
				return
			}

			saveRepoState(state, &stateMu, repo, updatedRepoState)
		}(repo)
	}

	wg.Wait()
}

func checkRepo(
	ctx context.Context,
	client *http.Client,
	cfg *Config,
	repo RepoConfig,
	repoState RepoState,
	githubToken string,
	slackWebhookURL string,
) (RepoState, error) {
	if repoState.SeenReleaseIDs == nil {
		repoState.SeenReleaseIDs = map[int64]SeenRelease{}
	}

	releases, err := fetchGitHubReleases(ctx, client, githubToken, repo.owner, repo.name)
	if err != nil {
		return repoState, err
	}

	sort.Slice(releases, func(i, j int) bool {
		return releaseTime(releases[i]).Before(releaseTime(releases[j]))
	})

	resolvedDefaultExcludePrereleases := boolValue(cfg.Defaults.ExcludePrereleases, defaultExcludePrerelease)
	resolvedDefaultExcludeUpdates := boolValue(cfg.Defaults.ExcludeUpdates, defaultExcludeUpdates)
	resolvedDefaultNotifyOnFirstRun := boolValue(cfg.Defaults.NotifyOnFirstRun, defaultNotifyOnFirstRun)

	excludePrereleases := boolValue(repo.ExcludePrereleases, resolvedDefaultExcludePrereleases)
	excludeUpdates := boolValue(repo.ExcludeUpdates, resolvedDefaultExcludeUpdates)
	notifyOnFirstRun := boolValue(repo.NotifyOnFirstRun, resolvedDefaultNotifyOnFirstRun)

	firstRun := !repoState.Initialized

	for _, release := range releases {
		if release.Draft {
			continue
		}

		if excludePrereleases && release.Prerelease {
			continue
		}

		seenRelease, alreadySeen := repoState.SeenReleaseIDs[release.ID]
		releaseUpdated := alreadySeen && release.UpdatedAt.After(seenRelease.UpdatedAt)

		if alreadySeen && !releaseUpdated {
			continue
		}

		if releaseUpdated && excludeUpdates {
			log.Printf("skipping update for %s/%s %s", repo.owner, repo.name, release.TagName)
			markSeen(repoState, release)
			continue
		}

		if firstRun && !notifyOnFirstRun {
			log.Printf("bootstrap: marking %s/%s %s as seen without notification", repo.owner, repo.name, release.TagName)
			markSeen(repoState, release)
			continue
		}

		msg := formatSlackMessage(repo, release, releaseUpdated)

		if err := sendSlackMessage(ctx, client, slackWebhookURL, msg); err != nil {
			return repoState, fmt.Errorf("failed to send slack notification for %s/%s %s: %w",
				repo.owner,
				repo.name,
				release.TagName,
				err,
			)
		}

		markSeen(repoState, release)
		if releaseUpdated {
			log.Printf("notified release update %s/%s %s", repo.owner, repo.name, release.TagName)
		} else {
			log.Printf("notified release %s/%s %s", repo.owner, repo.name, release.TagName)
		}
	}

	repoState.Initialized = true

	return repoState, nil
}

func repoStateSnapshot(state *State, stateMu *sync.Mutex, repo RepoConfig) RepoState {
	stateMu.Lock()
	defer stateMu.Unlock()

	if state.Repos == nil {
		state.Repos = map[string]RepoState{}
	}

	return cloneRepoState(state.Repos[repoKey(repo.owner, repo.name)])
}

func saveRepoState(state *State, stateMu *sync.Mutex, repo RepoConfig, repoState RepoState) {
	stateMu.Lock()
	defer stateMu.Unlock()

	if state.Repos == nil {
		state.Repos = map[string]RepoState{}
	}

	state.Repos[repoKey(repo.owner, repo.name)] = repoState
}

func cloneRepoState(repoState RepoState) RepoState {
	clone := RepoState{
		Initialized:    repoState.Initialized,
		SeenReleaseIDs: map[int64]SeenRelease{},
	}

	for releaseID, seenRelease := range repoState.SeenReleaseIDs {
		clone.SeenReleaseIDs[releaseID] = seenRelease
	}

	return clone
}

func fetchGitHubReleases(
	ctx context.Context,
	client *http.Client,
	token string,
	owner string,
	repo string,
) ([]GitHubRelease, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/releases?per_page=30", strings.TrimRight(githubAPIBaseURL, "/"), owner, repo)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "github-release-notifier")

	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, githubAPIError(resp, owner, repo)
	case http.StatusForbidden:
		return nil, githubAPIError(resp, owner, repo)
	default:
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, githubAPIError(resp, owner, repo)
		}
	}

	var releases []GitHubRelease
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return nil, err
	}

	return releases, nil
}

func githubAPIError(resp *http.Response, owner string, repo string) error {
	var githubErr struct {
		Message          string `json:"message"`
		DocumentationURL string `json:"documentation_url"`
	}

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxGitHubErrorBodyBytes))
	if readErr == nil && len(bytes.TrimSpace(body)) > 0 {
		_ = json.Unmarshal(body, &githubErr)
	}

	details := []string{
		fmt.Sprintf("github api returned status %s for %s/%s", resp.Status, owner, repo),
	}

	if githubErr.Message != "" {
		details = append(details, fmt.Sprintf("message=%q", githubErr.Message))
	}

	if githubErr.DocumentationURL != "" {
		details = append(details, fmt.Sprintf("docs=%s", githubErr.DocumentationURL))
	}

	if remaining := headerValue(resp.Header, "X-RateLimit-Remaining"); remaining != "" {
		details = append(details, "rate_limit_remaining="+remaining)
	}

	if reset := headerValue(resp.Header, "X-RateLimit-Reset"); reset != "" {
		if unixReset, err := strconv.ParseInt(reset, 10, 64); err == nil {
			details = append(details, "rate_limit_reset="+time.Unix(unixReset, 0).UTC().Format(time.RFC3339))
		} else {
			details = append(details, "rate_limit_reset="+reset)
		}
	}

	if resource := headerValue(resp.Header, "X-RateLimit-Resource"); resource != "" {
		details = append(details, "rate_limit_resource="+resource)
	}

	if readErr != nil {
		details = append(details, fmt.Sprintf("failed_to_read_error_body=%v", readErr))
	} else if githubErr.Message == "" && len(bytes.TrimSpace(body)) > 0 {
		details = append(details, fmt.Sprintf("body=%q", strings.TrimSpace(string(body))))
	}

	return errors.New(strings.Join(details, ", "))
}

func headerValue(header http.Header, name string) string {
	if value := header.Get(name); value != "" {
		return value
	}

	for key, values := range header {
		if !strings.EqualFold(key, name) || len(values) == 0 {
			continue
		}

		return values[0]
	}

	return ""
}

func sendSlackMessage(ctx context.Context, client *http.Client, webhookURL string, text string) error {
	payload := SlackMessage{
		Text: text,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(body))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("slack webhook returned status %s", resp.Status)
	}

	return nil
}

func formatSlackMessage(repo RepoConfig, release GitHubRelease, isUpdate bool) string {
	releaseName := strings.TrimSpace(release.Name)
	if releaseName == "" {
		releaseName = release.TagName
	}

	action := "New"
	if isUpdate {
		action = "Updated"
	}

	return fmt.Sprintf(
		":rocket: *%s GitHub release* for *%s/%s*\n*<%s|%s>* (`%s`)%s\n%s",
		action,
		repo.owner,
		repo.name,
		release.HTMLURL,
		releaseName,
		release.TagName,
		formatReleaseNotesSection(release.Body),
		release.HTMLURL,
	)
}

func formatReleaseNotesSection(body string) string {
	notes := formatReleaseNotes(body, maxReleaseNotesRunes)
	if notes == "" {
		return ""
	}

	return "\n\n*Release notes:*\n" + quoteSlackBlock(notes)
}

func formatReleaseNotes(body string, maxRunes int) string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")

	lines := strings.Split(body, "\n")
	normalized := make([]string, 0, len(lines))
	blankLines := 0

	for _, line := range lines {
		line = strings.TrimRight(line, " \t")
		if strings.TrimSpace(line) == "" {
			blankLines++
			if blankLines <= maxBlankReleaseNoteLines {
				normalized = append(normalized, "")
			}
			continue
		}

		blankLines = 0
		normalized = append(normalized, line)
	}

	notes := strings.TrimSpace(strings.Join(normalized, "\n"))
	if notes == "" {
		return ""
	}

	return truncateRunes(notes, maxRunes)
}

func quoteSlackBlock(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if line == "" {
			lines[i] = ">"
			continue
		}

		lines[i] = "> " + line
	}

	return strings.Join(lines, "\n")
}

func truncateRunes(text string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}

	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text
	}

	return strings.TrimSpace(string(runes[:maxRunes])) + releaseNotesTruncation
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	applyConfigDefaults(&cfg)

	if err := normalizeRepoConfigs(cfg.Repos); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func applyConfigDefaults(cfg *Config) {
	if cfg.CheckInterval == 0 {
		cfg.CheckInterval = defaultCheckInterval
	}

	if cfg.MaxConcurrentChecks <= 0 {
		cfg.MaxConcurrentChecks = defaultMaxConcurrentChecks
	}

	if cfg.StateFile == "" {
		cfg.StateFile = defaultStateFile
	}

}

func normalizeRepoConfigs(repos []RepoConfig) error {
	seenRepos := map[string]int{}

	for i := range repos {
		if strings.TrimSpace(repos[i].URL) == "" {
			return fmt.Errorf("repos[%d].url is required", i)
		}

		owner, name, err := parseGitHubRepoURL(repos[i].URL)
		if err != nil {
			return fmt.Errorf("repos[%d].url: %w", i, err)
		}

		repos[i].owner = owner
		repos[i].name = name

		key := repoKey(owner, name)
		if firstIndex, ok := seenRepos[key]; ok {
			log.Printf("duplicate repo config: repos[%d] duplicates repos[%d] for %s", i, firstIndex, key)
			continue
		}

		seenRepos[key] = i
	}

	return nil
}

func parseGitHubRepoURL(rawURL string) (string, string, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", "", err
	}

	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return "", "", fmt.Errorf("unsupported scheme %q", parsed.Scheme)
	}

	if !strings.EqualFold(parsed.Host, "github.com") {
		return "", "", fmt.Errorf("unsupported host %q", parsed.Host)
	}

	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", errors.New("expected GitHub repository URL in the form https://github.com/owner/repo")
	}

	return parts[0], strings.TrimSuffix(parts[1], ".git"), nil
}

func loadState(path string) (*State, error) {
	state := &State{
		Repos: map[string]RepoState{},
	}

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return state, nil
	}

	if err != nil {
		return nil, err
	}

	if len(bytes.TrimSpace(data)) == 0 {
		return state, nil
	}

	if err := json.Unmarshal(data, state); err != nil {
		return nil, err
	}

	if state.Repos == nil {
		state.Repos = map[string]RepoState{}
	}

	return state, nil
}

func pruneState(state *State, repos []RepoConfig) int {
	if state.Repos == nil {
		return 0
	}

	configuredRepos := map[string]struct{}{}
	for _, repo := range repos {
		if repo.owner == "" || repo.name == "" {
			continue
		}

		configuredRepos[repoKey(repo.owner, repo.name)] = struct{}{}
	}

	removed := 0
	for key := range state.Repos {
		if _, ok := configuredRepos[key]; ok {
			continue
		}

		delete(state.Repos, key)
		removed++
	}

	return removed
}

func saveState(path string, state *State) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}

	tmp := path + ".tmp"

	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}

	return os.Rename(tmp, path)
}

func markSeen(repoState RepoState, release GitHubRelease) {
	firstSeenAt := time.Now().UTC()
	if seenRelease, ok := repoState.SeenReleaseIDs[release.ID]; ok {
		firstSeenAt = seenRelease.FirstSeenAt
	}

	repoState.SeenReleaseIDs[release.ID] = SeenRelease{
		TagName:     release.TagName,
		FirstSeenAt: firstSeenAt,
		UpdatedAt:   release.UpdatedAt,
	}
}

func repoKey(owner string, repo string) string {
	return owner + "/" + repo
}

func releaseTime(release GitHubRelease) time.Time {
	if release.PublishedAt != nil {
		return *release.PublishedAt
	}

	return release.CreatedAt
}

func boolValue(value *bool, fallback bool) bool {
	if value == nil {
		return fallback
	}

	return *value
}

func getenvDefault(name string, fallback string) string {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}

	return value
}
