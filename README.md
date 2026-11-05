# github-release-notifier

Slack notifier for new GitHub releases.

The notifier uses the GitHub releases API, so tags without a GitHub Release are ignored. Draft releases are always skipped. Pre-releases and updates to already seen releases can be skipped globally in `defaults` or per repository.

## Configuration

Runtime configuration is read from `CONFIG_FILE`. See `example-config.yaml` for all supported config parameters and their default values.

Environment variables:

Slack delivery - set one of the following:

- `SLACK_BOT_TOKEN` + `SLACK_CHANNEL` - Slack bot token (`xoxb-...`) and target channel (channel ID such as `C0123456789` or a name such as `#releases`). Messages are sent with the Web API method `chat.postMessage`. The Slack app needs the `chat:write` scope and the bot must be a member of the channel (or use `chat:write.public` for public channels). Takes precedence over `SLACK_WEBHOOK_URL` when both are set.
- `SLACK_WEBHOOK_URL` - Slack incoming webhook URL used to send notifications.

Other:

- `CONFIG_FILE` - optional. Path to the YAML config file. Defaults to `/config/config.yaml`.
- `GITHUB_TOKEN` - optional. GitHub API token. Public repositories can be checked without it, but using a token gives higher API rate limits and is required for private repositories.

Example:

```sh
CONFIG_FILE=./config.yaml \
SLACK_BOT_TOKEN='xoxb-...' \
SLACK_CHANNEL='#releases' \
GITHUB_TOKEN='github_pat_...' \
go run .
```

Or with an incoming webhook:

```sh
CONFIG_FILE=./config.yaml \
SLACK_WEBHOOK_URL='https://hooks.slack.com/services/...' \
GITHUB_TOKEN='github_pat_...' \
go run .
```
