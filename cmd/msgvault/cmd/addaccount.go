package cmd

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/wesm/msgvault/internal/oauth"
	"github.com/wesm/msgvault/internal/store"
)

var (
	headless           bool
	accountDisplayName string
	forceReauth        bool
	oauthAppName       string
)

var addAccountCmd = &cobra.Command{
	Use:   "add-account <email>",
	Short: "Add a Gmail account via OAuth",
	Long: `Add a Gmail account by completing the OAuth2 authorization flow.

By default, opens a browser for authorization. Use --headless to see instructions
for authorizing on headless servers (Google does not support Gmail in device flow).

If a token already exists, the command skips authorization. Use --force to delete
the existing token and start a fresh OAuth flow.

For Google Workspace orgs that require their own OAuth app, use --oauth-app to
specify a named app from config.toml.

Examples:
  msgvault add-account you@gmail.com
  msgvault add-account you@gmail.com --headless
  msgvault add-account you@gmail.com --force
  msgvault add-account you@acme.com --oauth-app acme
  msgvault add-account you@gmail.com --display-name "Work Account"`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		email := args[0]

		if headless && forceReauth {
			return fmt.Errorf("--headless and --force cannot be used together: --force requires browser-based OAuth which is not available in headless mode")
		}

		// For --headless, just show instructions (no OAuth config needed)
		if headless {
			oauth.PrintHeadlessInstructions(email, cfg.TokensDir(), oauthAppName)
			return nil
		}

		// Resolve which client secrets to use
		resolvedApp := oauthAppName
		var clientSecretsPath string

		// Initialize database (in case it's new)
		dbPath := cfg.DatabaseDSN()
		s, err := store.Open(dbPath)
		if err != nil {
			return fmt.Errorf("open database: %w", err)
		}
		defer s.Close()

		if err := s.InitSchema(); err != nil {
			return fmt.Errorf("init schema: %w", err)
		}

		// Look up existing source to detect binding changes
		existingSource, err := findGmailSource(s, email)
		if err != nil {
			return fmt.Errorf("look up existing source: %w", err)
		}

		// For --force without --oauth-app, inherit existing binding
		if forceReauth && resolvedApp == "" && existingSource != nil && existingSource.OAuthApp.Valid {
			resolvedApp = existingSource.OAuthApp.String
		}

		// Detect binding change
		bindingChanged := false
		if existingSource != nil && oauthAppName != "" {
			currentApp := ""
			if existingSource.OAuthApp.Valid {
				currentApp = existingSource.OAuthApp.String
			}
			if currentApp != oauthAppName {
				bindingChanged = true
			}
		}

		// Resolve client secrets path
		clientSecretsPath, err = cfg.OAuth.ClientSecretsFor(resolvedApp)
		if err != nil {
			if !cfg.OAuth.HasAnyConfig() {
				return errOAuthNotConfigured()
			}
			return err
		}

		// Create OAuth manager
		oauthMgr, err := oauth.NewManager(clientSecretsPath, cfg.TokensDir(), logger)
		if err != nil {
			return wrapOAuthError(fmt.Errorf("create oauth manager: %w", err))
		}

		// Handle binding change: delete old token and re-auth
		if bindingChanged {
			fmt.Printf("Switching OAuth app for %s to %q. Re-authorizing...\n", email, oauthAppName)
			if oauthMgr.HasToken(email) {
				if err := oauthMgr.DeleteToken(email); err != nil {
					return fmt.Errorf("delete existing token: %w", err)
				}
			}
			// Update binding in DB
			newApp := sql.NullString{String: oauthAppName, Valid: true}
			if err := s.UpdateSourceOAuthApp(existingSource.ID, newApp); err != nil {
				return fmt.Errorf("update oauth app binding: %w", err)
			}
		}

		// If --force, delete existing token so we re-authorize
		if forceReauth && !bindingChanged {
			if oauthMgr.HasToken(email) {
				fmt.Printf("Removing existing token for %s...\n", email)
				if err := oauthMgr.DeleteToken(email); err != nil {
					return fmt.Errorf("delete existing token: %w", err)
				}
			} else {
				fmt.Printf("No existing token found for %s, proceeding with authorization.\n", email)
			}
		}

		// Check if already authorized (skip if binding just changed)
		if !bindingChanged && oauthMgr.HasToken(email) {
			source, err := s.GetOrCreateSource("gmail", email)
			if err != nil {
				return fmt.Errorf("create source: %w", err)
			}
			// Set oauth_app on new source if specified
			if oauthAppName != "" && !source.OAuthApp.Valid {
				newApp := sql.NullString{String: oauthAppName, Valid: true}
				if err := s.UpdateSourceOAuthApp(source.ID, newApp); err != nil {
					return fmt.Errorf("update oauth app binding: %w", err)
				}
			}
			if accountDisplayName != "" {
				if err := s.UpdateSourceDisplayName(source.ID, accountDisplayName); err != nil {
					return fmt.Errorf("set display name: %w", err)
				}
			}
			fmt.Printf("Account %s is already authorized.\n", email)
			fmt.Println("Next step: msgvault sync-full", email)
			return nil
		}

		// Perform authorization
		fmt.Println("Starting browser authorization...")

		if err := oauthMgr.Authorize(cmd.Context(), email); err != nil {
			var mismatch *oauth.TokenMismatchError
			if errors.As(err, &mismatch) {
				existing, lookupErr := findGmailSource(s, email)
				if lookupErr != nil {
					return fmt.Errorf("authorization failed: %w (also: %v)", err, lookupErr)
				}
				if existing == nil {
					return fmt.Errorf(
						"%w\nIf %s is the primary address, re-add with:\n"+
							"  msgvault add-account %s",
						err, mismatch.Actual, mismatch.Actual,
					)
				}
			}
			return fmt.Errorf("authorization failed: %w", err)
		}

		// Create source record in database
		source, err := s.GetOrCreateSource("gmail", email)
		if err != nil {
			return fmt.Errorf("create source: %w", err)
		}

		// Set oauth_app binding
		if oauthAppName != "" {
			newApp := sql.NullString{String: oauthAppName, Valid: true}
			if err := s.UpdateSourceOAuthApp(source.ID, newApp); err != nil {
				return fmt.Errorf("update oauth app binding: %w", err)
			}
		}

		if accountDisplayName != "" {
			if err := s.UpdateSourceDisplayName(source.ID, accountDisplayName); err != nil {
				return fmt.Errorf("set display name: %w", err)
			}
		}

		fmt.Printf("\nAccount %s authorized successfully!\n", email)
		fmt.Println("You can now run: msgvault sync-full", email)

		return nil
	},
}

func findGmailSource(
	s *store.Store, email string,
) (*store.Source, error) {
	sources, err := s.GetSourcesByIdentifier(email)
	if err != nil {
		return nil, fmt.Errorf("look up sources for %s: %w", email, err)
	}
	for _, src := range sources {
		if src.SourceType == "gmail" {
			return src, nil
		}
	}
	return nil, nil
}

func init() {
	addAccountCmd.Flags().BoolVar(&headless, "headless", false, "Show instructions for headless server setup")
	addAccountCmd.Flags().BoolVar(&forceReauth, "force", false, "Delete existing token and re-authorize")
	addAccountCmd.Flags().StringVar(&accountDisplayName, "display-name", "", "Display name for the account (e.g., \"Work\", \"Personal\")")
	addAccountCmd.Flags().StringVar(&oauthAppName, "oauth-app", "", "Named OAuth app from config (for Google Workspace orgs)")
	rootCmd.AddCommand(addAccountCmd)
}
