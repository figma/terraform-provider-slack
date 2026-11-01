package slack

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/retry"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/slack-go/slack"
)

type userGroupCache struct {
	mu      sync.Mutex
	entries map[bool]*userGroupRequest
}

type userGroupRequest struct {
	ready  chan struct{}
	groups []slack.UserGroup
	err    error
}

func ListUserGroupsWithContext(
	ctx context.Context,
	d *schema.ResourceData,
	m interface{},
	includeDisabled bool,
) ([]slack.UserGroup, error) {
	client := m.(*providerClient)
	cache := &client.userGroups
	cache.mu.Lock()
	entry, exists := cache.entries[includeDisabled]
	if !exists {
		entry = &userGroupRequest{ready: make(chan struct{})}
		if cache.entries == nil {
			cache.entries = make(map[bool]*userGroupRequest)
		}
		cache.entries[includeDisabled] = entry
	}
	cache.mu.Unlock()

	if exists {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-entry.ready:
			return entry.groups, entry.err
		}
	}

	groups, err := fetchUserGroupsWithContext(ctx, d, client.Client, includeDisabled)
	cache.mu.Lock()
	entry.groups, entry.err = groups, err
	if err != nil && cache.entries[includeDisabled] == entry {
		delete(cache.entries, includeDisabled)
	}
	close(entry.ready)
	cache.mu.Unlock()
	return groups, err
}

func fetchUserGroupsWithContext(
	ctx context.Context,
	d *schema.ResourceData,
	client *slack.Client,
	includeDisabled bool,
) ([]slack.UserGroup, error) {
	var (
		userGroups []slack.UserGroup
		//nolint:gomnd
		backoff = &Backoff{Base: time.Second, Cap: 15 * time.Second}
	)
	err := retry.RetryContext(ctx, d.Timeout(schema.TimeoutRead), func() *retry.RetryError {
		var (
			err   error
			rlerr *slack.RateLimitedError
		)

		userGroups, err = client.GetUserGroupsContext(
			ctx,
			slack.GetUserGroupsOptionIncludeDisabled(includeDisabled),
			slack.GetUserGroupsOptionIncludeUsers(true),
		)

		if errors.As(err, &rlerr) {
			backoff.Sleep(ctx)
			return retry.RetryableError(err)
		}

		if err != nil {
			return retry.NonRetryableError(fmt.Errorf("couldn't get usergroups: %w", err))
		}

		return nil
	})

	return userGroups, err
}

func (c *providerClient) invalidateUserGroups() {
	c.userGroups.mu.Lock()
	c.userGroups.entries = nil
	c.userGroups.mu.Unlock()
}

func (c *providerClient) CreateUserGroupContext(ctx context.Context, group slack.UserGroup) (slack.UserGroup, error) {
	defer c.invalidateUserGroups()
	return c.Client.CreateUserGroupContext(ctx, group)
}

func (c *providerClient) EnableUserGroupContext(ctx context.Context, id string) (slack.UserGroup, error) {
	defer c.invalidateUserGroups()
	return c.Client.EnableUserGroupContext(ctx, id)
}

func (c *providerClient) UpdateUserGroupContext(ctx context.Context, id string, options ...slack.UpdateUserGroupsOption) (slack.UserGroup, error) {
	defer c.invalidateUserGroups()
	return c.Client.UpdateUserGroupContext(ctx, id, options...)
}

func (c *providerClient) UpdateUserGroupMembersContext(ctx context.Context, id, members string) (slack.UserGroup, error) {
	defer c.invalidateUserGroups()
	return c.Client.UpdateUserGroupMembersContext(ctx, id, members)
}

func (c *providerClient) DisableUserGroupContext(ctx context.Context, id string) (slack.UserGroup, error) {
	defer c.invalidateUserGroups()
	return c.Client.DisableUserGroupContext(ctx, id)
}
