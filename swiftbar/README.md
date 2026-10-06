# SwiftBar quota status

Read-only quota dashboard with a gauge icon. Requires macOS 12+, [SwiftBar](https://github.com/swiftbar/SwiftBar) 2.1.0+, `jq` 1.6+, and installed `polytoken-quota` and `polytoken` executables. See the [CLI installation instructions](../README.md#install-and-initial-setup).

## Install

1. Install SwiftBar and `jq` (`brew install swiftbar jq`).
2. From the repository root, copy the plugin:

   ```sh
   mkdir -p "$HOME/SwiftBarPlugins"
   cp swiftbar/polytoken-quota.1m.sh "$HOME/SwiftBarPlugins/"
   chmod +x "$HOME/SwiftBarPlugins/polytoken-quota.1m.sh"
   ```

3. Select that folder in SwiftBar's **Plugin Folder** preferences. Enable **Launch SwiftBar on login** if desired.

## Configure

The plugin searches `PATH` and common Homebrew/user binary directories. If needed, create `polytoken-quota.conf` beside the installed script with absolute executable paths:

```ini
quota_bin=/usr/local/bin/polytoken-quota
polytoken_bin=/usr/local/bin/polytoken
jq_bin=/opt/homebrew/bin/jq
```

Adjust paths for your installation. Only these three keys are accepted; bad overrides do not fall back. The plugin uses your existing quota configuration and saved data. It never polls providers or reconciles; configure quota checks separately through the [CLI](../README.md).

The display refreshes every minute. After replacing the installed script, click **Refresh status**; no restart is needed. An amber gauge means attention is needed; run `polytoken-quota status` and `polytoken-quota doctor` for details.

To uninstall, remove the script and its optional `.conf` file from your SwiftBar plugin folder.
