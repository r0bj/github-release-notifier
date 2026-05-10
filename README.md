# github-release-notifier

Slack notifier for new GitHub releases.

The notifier uses the GitHub releases API, so tags without a GitHub Release are ignored. Draft releases are always skipped. Pre-releases and updates to already seen releases can be skipped globally in `defaults` or per repository.

## Configuration

Runtime configuration is read from `CONFIG_FILE`. See `example-config.yaml` for all supported config parameters and their default values.

Environment variables:

- `SLACK_WEBHOOK_URL` - required. Slack incoming webhook URL used to send notifications.
- `CONFIG_FILE` - optional. Path to the YAML config file. Defaults to `/config/config.yaml`.
- `GITHUB_TOKEN` - optional. GitHub API token. Public repositories can be checked without it, but using a token gives higher API rate limits and is required for private repositories.

Example:

```sh
CONFIG_FILE=./config.yaml \
SLACK_WEBHOOK_URL='https://hooks.slack.com/services/...' \
GITHUB_TOKEN='github_pat_...' \
go run .
```
