# FDL daemon LaunchAgent

Use `com.multica.fdl-daemon.plist.example` to keep one local native FDL
executor running across login and daemon crashes. Render it into the current
user's `~/Library/LaunchAgents/` directory; do not commit the rendered file.

Replace every placeholder with an absolute path:

- `__MULTICA_CLI__`: a pinned, installed Multica CLI binary with a released
  semver version.
- `__MULTICA_PROFILE__`: the dedicated daemon profile name.
- `__FDL_CLI__` and `__FDL_RUN_ROOT__`: the installed Controller and external
  run root. The run root must remain outside every bound worktree.
- `__PATH__`: include the pinned gate environment and agent CLI locations.
- `__STDOUT_LOG__` and `__STDERR_LOG__`: user-local logs outside the repo.

The LaunchAgent must contain no API token, webhook, or adapter configuration.
Those remain in the CLI profile and the owner-only adapter config. Validate
the rendered plist with `plutil -lint`, then load it with:

```sh
launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/com.multica.fdl-daemon.plist"
launchctl kickstart -k "gui/$(id -u)/com.multica.fdl-daemon"
```

To replace an installed definition, first run
`launchctl bootout "gui/$(id -u)/com.multica.fdl-daemon"`. Confirm the daemon
health endpoint and its registered runtime after every replacement.
