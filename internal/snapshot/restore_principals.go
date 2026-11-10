package snapshot

import (
	"context"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/snapshot/migrations"
)

// users, then token_limits: first, before any definition or resumed run.
func restoreUsersSection(ctx context.Context, rs *restoreRun) error {
	s, sections, result := rs.s, rs.sections, rs.result
	if rawSection, ok := sections[migrations.SectionUsers]; ok {
		var sec UsersSection
		if err := decodeWithMigration(migrations.SectionUsers, rawSection, &sec); err != nil {
			return err
		}
		if err := restoreBootstraps(ctx, s, &sec); err != nil {
			return err
		}
		restoreUsers(ctx, s, &sec, result)
	}
	return nil
}

// restoreTokenLimitsSection restores the token_limits section (its place: restoreSectionOrder).
func restoreTokenLimitsSection(ctx context.Context, rs *restoreRun) error {
	s, opts, sections, result := rs.s, rs.opts, rs.sections, rs.result
	if rawSection, ok := sections[migrations.SectionTokenLimits]; ok {
		var sec TokenLimitsSection
		if err := decodeWithMigration(migrations.SectionTokenLimits, rawSection, &sec); err != nil {
			return err
		}
		now := time.Now
		if opts.Now != nil {
			now = opts.Now
		}
		restoreTokenLimits(ctx, s, &sec, now(), result)
	}
	return nil
}

// acl: after the users its memberships and bindings name, and before the
// paused and queued runs, which are re-narrowed by their caller's set.
func restoreACLSection(ctx context.Context, rs *restoreRun) error {
	s, sections, result := rs.s, rs.sections, rs.result
	if rawSection, ok := sections[migrations.SectionACL]; ok {
		var sec ACLSection
		if err := decodeWithMigration(migrations.SectionACL, rawSection, &sec); err != nil {
			return err
		}
		restoreACL(ctx, s, &sec, result)
	}
	return nil
}

// oauth_credentials: before the definitions that name them, so the
// missing-credential scan finds each one, not connected.
func restoreOAuthCredentialsSection(ctx context.Context, rs *restoreRun) error {
	s, sections, result := rs.s, rs.sections, rs.result
	if rawSection, ok := sections[migrations.SectionOAuthCredentials]; ok {
		var sec OAuthCredentialsSection
		if err := decodeWithMigration(migrations.SectionOAuthCredentials, rawSection, &sec); err != nil {
			return err
		}
		restoreOAuthCredentials(ctx, s, &sec, result)
	}
	return nil
}
