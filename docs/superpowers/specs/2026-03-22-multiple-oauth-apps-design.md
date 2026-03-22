# Multiple OAuth Apps Support

## Problem

Some Google Workspace organizations require OAuth apps to live within their org. A personal OAuth app cannot authorize accounts in those orgs. msgvault currently supports only a single OAuth client secret, so users with accounts across multiple Workspace orgs (or a mix of personal and org accounts) cannot archive all of them.

## Design

### Config Format

Backward-compatible extension of `config.toml`. The existing `[oauth].client_secrets` field continues to work as the default. Named OAuth apps are added under `[oauth.apps.<name>]`:

```toml
[oauth]
client_secrets = "/path/to/default_secret.json"

[oauth.apps.acme]
client_secrets = "/path/to/acme_workspace_secret.json"

[oauth.apps.personal]
client_secrets = "/path/to/personal_secret.json"
```

Single-app users never see the `[oauth.apps]` section. Their config works unchanged.

**The top-level `client_secrets` is not mandatory.** A config with only named apps and no default is valid:

```toml
[oauth]
# no default client_secrets

[oauth.apps.acme]
client_secrets = "/path/to/acme_secret.json"

[oauth.apps.personal]
client_secrets = "/path/to/personal_secret.json"
```

This means the "OAuth not configured" checks in commands (`cfg.OAuth.ClientSecrets == ""`) must change. The check should be: "is there any OAuth config at all?" — i.e., `client_secrets` is set OR `apps` has at least one entry. Per-account resolution then determines which credentials to use:

1. Look up `sources.oauth_app` for the account.
2. If non-null, resolve via `ClientSecretsFor(name)`.
3. If null, fall back to `ClientSecrets` (the default). If the default is also empty, return an error telling the user to either set `[oauth].client_secrets` or run `add-account --oauth-app <name>`.

The early "OAuth not configured" guard (`errOAuthNotConfigured()`) must be deferred until per-account resolution, not checked eagerly at command start. Commands that loop over all accounts should skip Gmail accounts that have no resolvable OAuth config rather than failing the entire command.

**Go structs:**

```go
type OAuthConfig struct {
    ClientSecrets string              `toml:"client_secrets"`
    Apps          map[string]OAuthApp `toml:"apps"`
}

type OAuthApp struct {
    ClientSecrets string `toml:"client_secrets"`
}
```

A helper method resolves the correct path:

```go
func (o *OAuthConfig) ClientSecretsFor(name string) (string, error)
```

- Empty name returns `o.ClientSecrets` (the default). Returns an error if the default is empty.
- Non-empty name looks up `o.Apps[name]` and returns its `ClientSecrets`. Returns an error if the name is not found or its path is empty.
- All errors include actionable config guidance.

A second helper checks whether any OAuth config exists at all:

```go
func (o *OAuthConfig) HasAnyConfig() bool
```

Returns true if `ClientSecrets` is non-empty OR `Apps` has at least one entry with a non-empty `ClientSecrets`. Used to replace the current `cfg.OAuth.ClientSecrets == ""` guards.

**Path normalization:** The `Load()` function must apply `expandPath` and `resolveRelative` to each `Apps[*].ClientSecrets` entry, same as it does for the top-level `ClientSecrets`. This ensures `~` expansion and relative path resolution work for named apps.

### Schema Migration

Add a nullable `oauth_app` column to the `sources` table:

```sql
ALTER TABLE sources ADD COLUMN oauth_app TEXT;
```

- `NULL` means "use the default `[oauth].client_secrets`".
- A non-null value (e.g., `"acme"`) maps to `[oauth.apps.acme]`.

The `Source` struct gains a new field:

```go
type Source struct {
    // ... existing fields ...
    OAuthApp sql.NullString
}
```

All queries that read from `sources` must include `oauth_app` in their column list.

### CLI: `add-account --oauth-app`

The `add-account` command gains an `--oauth-app` flag:

```bash
# Default app (unchanged)
msgvault add-account you@gmail.com

# Named app
msgvault add-account you@acme.com --oauth-app acme
```

**Behavior:**

1. If `--oauth-app` is set, validate the named app exists in config. Use its `client_secrets` to create the `oauth.Manager`. Persist the name to `sources.oauth_app`.
2. If `--oauth-app` is omitted, use the top-level `[oauth].client_secrets` as today. Store `NULL` in `sources.oauth_app`.
3. If `--force` re-auth is used on an existing account, look up the existing `oauth_app` binding from the DB so the user doesn't need to re-specify it. If `--oauth-app` is also provided, it overrides the stored binding.

**Changing an account's OAuth app binding:**

When `add-account` is run on an existing account with `--oauth-app`, and the new value differs from the stored `sources.oauth_app`:

1. Update `sources.oauth_app` to the new value.
2. Delete the existing token (`oauthMgr.DeleteToken`).
3. Run the full authorization flow with the new app's credentials.

This is necessary because the current token file has no client identity metadata — there is no way to verify whether an existing token was minted by the same OAuth client. A token minted by one client will fail to refresh against a different client's credentials. Rather than adding client metadata to the token file for a rare migration operation, we simply re-auth. The user experience is: "Switching OAuth app for you@acme.com. Re-authorizing..."

The `HasToken` short-circuit in `add-account` must be restructured: check for an oauth_app binding change **before** checking `HasToken`, so that a binding change triggers re-auth even when a token exists.

**Migration path for existing users** switching from single to multiple apps: run `add-account you@acme.com --oauth-app acme` on existing accounts. This detects the binding change (NULL to "acme"), deletes the old token, and re-authorizes with the new app's credentials.

**Error message** when a named app isn't found:

```
OAuth app "acme" not configured. Add it to config.toml:

  [oauth.apps.acme]
  client_secrets = "/path/to/client_secret.json"
```

### Sync-time Resolution

Replace the single `getOAuthMgr()` pattern with a lazy cache keyed by app name:

```go
oauthManagers := map[string]*oauth.Manager{}

getOAuthMgr := func(appName string) (*oauth.Manager, error) {
    if mgr, ok := oauthManagers[appName]; ok {
        return mgr, nil
    }
    secretsPath, err := cfg.OAuth.ClientSecretsFor(appName)
    if err != nil {
        return nil, err
    }
    mgr, err := oauth.NewManager(secretsPath, cfg.TokensDir(), logger)
    if err != nil {
        return nil, wrapOAuthError(err)
    }
    oauthManagers[appName] = mgr
    return mgr, nil
}
```

Callers read `source.OAuthApp` (converting `NullString` to `string`, where null becomes `""`) and pass it to `getOAuthMgr`.

**Affected commands:** `sync-full`, `sync`/`sync-incremental`, `serve`, `verify`, `deletions`. All follow the same `getOAuthMgr` pattern today, so the change is mechanical.

**Command-specific notes:**

- **`sync-full` and `sync` "unknown email" bootstrap**: Today, if the user passes an email not yet in the DB, these commands assume Gmail and proceed with the single OAuth manager. With named apps, there's no `sources.oauth_app` to look up. **Resolution:** when no source row exists, fall back to the default OAuth app (`[oauth].client_secrets`). If no default is configured, return an error telling the user to run `add-account <email> --oauth-app <name>` first. This is acceptable because the bootstrap path is a legacy convenience — the documented workflow is `add-account` then `sync`, and named-app users must use `add-account --oauth-app` anyway to establish the binding.
- **`serve`**: Currently creates a single `oauthMgr` eagerly and passes it into `runScheduledSync`. Must change to pass the cache (or a resolver function) so each scheduled account resolves its own manager. The scheduler receives an email string, so it needs to look up the source's `oauth_app` from the DB. **Fallback for missing source rows:** `serve` currently supports scheduled accounts that don't yet have a `sources` row (token uploaded first, account registered lazily via `GetOrCreateSource` during sync). When no source row exists, the resolver must fall back to the default OAuth app (`[oauth].client_secrets`). If no default is configured either, skip the account with a warning. This preserves the existing "upload token first" workflow while giving named-app users the correct resolution path once their accounts are registered.
- **`deletions`**: Uses `oauth.NewManagerWithScopes` with variable scopes (escalating to full access for `batchDelete`). The lazy cache should not be shared with the standard-scopes cache. Deletions already create their own manager instances per scope set — keep that pattern, just resolve the correct `client_secrets` path via `ClientSecretsFor`.

### Token Storage

No change. Tokens are stored per-account (`{email}.json`), not per-OAuth-app. The `sources` table tracks the binding; the token file doesn't need to know which app produced it.

If a user re-auths an account with a different OAuth app, the new token overwrites the old one. This is correct since only one binding is active per account.

### Documentation Updates

- **README.md**: add a section on multi-org OAuth setup with config example.
- **CLAUDE.md**: update the config example in the Configuration section.
- **`add-account` help text**: document the `--oauth-app` flag.
- **Setup wizard** (`setup.go`): no change for now (it handles the single-app path; multi-app is an advanced config).
- **Headless instructions** (`PrintHeadlessInstructions` in `oauth.go`): must accept an optional OAuth app name and include `--oauth-app <name>` in the printed commands when set. Both the browser-side step ("on a machine with a browser, run `msgvault add-account <email> --oauth-app <name>`") and the server-side step ("on the headless server, run `msgvault add-account <email> --oauth-app <name>`") need the flag, or headless setup will fail for org-bound accounts. The `add-account --headless` codepath must pass the `--oauth-app` value through to `PrintHeadlessInstructions`.
- **Error messages**: update `errOAuthNotConfigured()` and `oauthSetupHint()` to mention named apps when the user has `[oauth.apps]` configured.

## Scope

### In scope

- `OAuthConfig` struct expansion with `Apps` map and `ClientSecretsFor` helper
- Schema migration adding `oauth_app` column
- `Source` struct and all queries updated to include `oauth_app`
- `add-account --oauth-app` flag with update-existing-binding support
- Lazy manager cache in all sync/verify/deletion commands
- Config validation (named app exists, has non-empty `client_secrets`)
- Documentation updates

### Out of scope

- Domain-based auto-routing (accounts explicitly bind via `--oauth-app`)
- Changes to `add-imap` (IMAP uses its own credential storage)
- Client identity in token files (binding changes trigger re-auth instead)
- Setup wizard changes (multi-app is an advanced path)
- Per-app scopes (all apps use the same Gmail scopes)
