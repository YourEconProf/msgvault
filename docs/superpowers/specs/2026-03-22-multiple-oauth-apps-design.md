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

- Empty name returns `o.ClientSecrets` (the default).
- Non-empty name looks up `o.Apps[name]` and returns its `ClientSecrets`.
- Returns an error with actionable config guidance if the name is not found or the resolved path is empty.

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
3. If the account already exists and `--oauth-app` is provided, update `sources.oauth_app`. If the new app has different client credentials, the existing token won't refresh against the new client ID, so detect this and prompt for re-auth.
4. If `--force` re-auth is used on an existing account, look up the existing `oauth_app` binding from the DB so the user doesn't need to re-specify it.

**Migration path for existing users** switching from single to multiple apps: run `add-account you@acme.com --oauth-app acme` on existing accounts. This updates the binding and re-auths if needed.

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

### Token Storage

No change. Tokens are stored per-account (`{email}.json`), not per-OAuth-app. The `sources` table tracks the binding; the token file doesn't need to know which app produced it.

If a user re-auths an account with a different OAuth app, the new token overwrites the old one. This is correct since only one binding is active per account.

### Documentation Updates

- **README.md**: add a section on multi-org OAuth setup with config example.
- **CLAUDE.md**: update the config example in the Configuration section.
- **`add-account` help text**: document the `--oauth-app` flag.
- **Setup wizard** (`setup.go`): no change for now (it handles the single-app path; multi-app is an advanced config).
- **Headless instructions**: no change (token copying is app-agnostic).
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
- Changes to token file format
- Setup wizard changes (multi-app is an advanced path)
- Per-app scopes (all apps use the same Gmail scopes)
