package slack

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/slack-go/slack"
)

const slackErrorAlreadyDisabled = "already_disabled"

func resourceSlackUserGroup() *schema.Resource {
	return &schema.Resource{
		ReadContext:   resourceSlackUserGroupRead,
		CreateContext: resourceSlackUserGroupCreate,
		UpdateContext: resourceSlackUserGroupUpdate,
		DeleteContext: resourceSlackUserGroupDelete,

		Importer: &schema.ResourceImporter{
			StateContext: schema.ImportStatePassthroughContext,
		},

		Schema: map[string]*schema.Schema{
			"name": {
				Type:     schema.TypeString,
				Required: true,
			},
			"channels": {
				Type: schema.TypeSet,
				Elem: &schema.Schema{
					Type: schema.TypeString,
				},
				Set:      schema.HashString,
				Optional: true,
			},
			"description": {
				Type:     schema.TypeString,
				Optional: true,
			},
			"handle": {
				Type:     schema.TypeString,
				Optional: true,
			},
			"users": {
				Type: schema.TypeSet,
				Elem: &schema.Schema{
					Type: schema.TypeString,
				},
				Set:      schema.HashString,
				Optional: true,
			},
		},

		Timeouts: &schema.ResourceTimeout{
			//nolint:gomnd
			Read: schema.DefaultTimeout(5 * time.Minute),
		},
	}
}

func resourceSlackUserGroupCreate(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	client := m.(*providerClient)

	name := d.Get("name").(string)
	description := d.Get("description").(string)
	handle := d.Get("handle").(string)
	channels := d.Get("channels").(*schema.Set)
	users := d.Get("users").(*schema.Set)

	userGroup := slack.UserGroup{
		Name:        name,
		Description: description,
		Handle:      handle,
		Prefs: slack.UserGroupPrefs{
			Channels: schemaSetToSlice(channels),
		},
	}
	createdUserGroup, err := client.CreateUserGroupContext(ctx, userGroup)
	if err != nil {
		var (
			group          slack.UserGroup
			collisionField string
			collisionValue string
		)
		switch err.Error() {
		case "name_already_exists":
			group, err = findUserGroupByName(ctx, d, m, name, true)
			collisionField = "name"
			collisionValue = name
		case "handle_already_exists":
			group, err = findUserGroupByHandle(ctx, d, m, handle, true)
			collisionField = "handle"
			collisionValue = handle
		default:
			return diag.Errorf("could not create usergroup %s: %s", name, err)
		}
		if err != nil {
			return diag.Errorf("could not find existing usergroup for %s: %s", name, err)
		}
		if group.DateDelete == 0 {
			return diag.Errorf(
				"could not create usergroup %s: %s %s is already used by active usergroup %s (%s)",
				name,
				collisionField,
				collisionValue,
				group.Name,
				group.ID,
			)
		}

		d.SetId(group.ID)
		_, err = client.EnableUserGroupContext(ctx, group.ID)
		if err != nil {
			if err.Error() == "already_enabled" {
				d.SetId("")
				return diag.Errorf("could not restore usergroup %s (%s): usergroup became active before it could be restored", name, group.ID)
			}
			return diag.Errorf("could not enable usergroup %s (%s): %s", name, group.ID, err)
		}

		if users.Len() > 0 {
			_, err = client.UpdateUserGroupMembersContext(ctx, group.ID, strings.Join(schemaSetToSlice(users), ","))
			if err != nil {
				return rollbackRestoredUserGroup(ctx, d, client, fmt.Errorf("could not update restored usergroup members %s (%s): %w", name, group.ID, err))
			}
		}
		_, err = client.UpdateUserGroupContext(ctx, group.ID, userGroupUpdateOptions(name, description, handle, channels)...)
		if err != nil {
			return rollbackRestoredUserGroup(ctx, d, client, fmt.Errorf("could not update restored usergroup %s (%s): %w", name, group.ID, err))
		}
		return resourceSlackUserGroupRead(ctx, d, m)
	}

	d.SetId(createdUserGroup.ID)
	if users.Len() > 0 {
		_, err := client.UpdateUserGroupMembersContext(ctx, d.Id(), strings.Join(schemaSetToSlice(users), ","))
		if err != nil {
			return diag.Errorf("could not update usergroup members %s: %s", name, err)
		}
		schemaSetToSlice(users)
	}
	return resourceSlackUserGroupRead(ctx, d, m)
}

func rollbackRestoredUserGroup(
	ctx context.Context,
	d *schema.ResourceData,
	client *providerClient,
	restoreErr error,
) diag.Diagnostics {
	_, disableErr := client.DisableUserGroupContext(ctx, d.Id())
	if disableErr == nil || disableErr.Error() == slackErrorAlreadyDisabled {
		d.SetId("")
		return diag.FromErr(restoreErr)
	}
	return diag.Errorf("%s; could not disable usergroup %s again: %s", restoreErr, d.Id(), disableErr)
}

func resourceSlackUserGroupRead(
	ctx context.Context,
	d *schema.ResourceData,
	m interface{},
) diag.Diagnostics {
	id := d.Id()

	userGroups, err := ListUserGroupsWithContext(ctx, d, m, true)

	if err != nil {
		return diag.FromErr(err)
	}

	var diags diag.Diagnostics

	for _, userGroup := range userGroups {
		if userGroup.ID == id {
			return updateUserGroupData(d, userGroup)
		}
	}
	diags = append(diags, diag.Diagnostic{
		Severity: diag.Warning,
		Summary:  fmt.Sprintf("usergroup with ID %s not found, removing from state", id),
	})

	d.SetId("")

	return diags
}

func findUserGroupByName(
	ctx context.Context,
	d *schema.ResourceData,
	m interface{},
	name string,
	includeDisabled bool,
) (slack.UserGroup, error) {
	userGroups, err := ListUserGroupsWithContext(ctx, d, m, includeDisabled)
	if err != nil {
		return slack.UserGroup{}, err
	}

	for _, userGroup := range userGroups {
		if userGroup.Name == name {
			return userGroup, nil
		}
	}

	return slack.UserGroup{}, fmt.Errorf("could not find usergroup %s", name)
}

func findUserGroupByID(
	ctx context.Context,
	d *schema.ResourceData,
	m interface{},
	id string,
	includeDisabled bool,
) (slack.UserGroup, error) {
	userGroups, err := ListUserGroupsWithContext(ctx, d, m, includeDisabled)
	if err != nil {
		return slack.UserGroup{}, err
	}

	for _, userGroup := range userGroups {
		if userGroup.ID == id {
			return userGroup, nil
		}
	}

	return slack.UserGroup{}, fmt.Errorf("could not find usergroup %s", id)
}

func findUserGroupByHandle(
	ctx context.Context,
	d *schema.ResourceData,
	m interface{},
	handle string,
	includeDisabled bool,
) (slack.UserGroup, error) {
	userGroups, err := ListUserGroupsWithContext(ctx, d, m, includeDisabled)
	if err != nil {
		return slack.UserGroup{}, err
	}

	for _, userGroup := range userGroups {
		if userGroup.Handle == handle {
			return userGroup, nil
		}
	}

	return slack.UserGroup{}, fmt.Errorf("could not find usergroup with handle %s", handle)
}

func userGroupUpdateOptions(
	name string,
	description string,
	handle string,
	channels *schema.Set,
) []slack.UpdateUserGroupsOption {
	return []slack.UpdateUserGroupsOption{
		slack.UpdateUserGroupsOptionName(name),
		slack.UpdateUserGroupsOptionChannels(schemaSetToSlice(channels)),
		slack.UpdateUserGroupsOptionDescription(&description),
		slack.UpdateUserGroupsOptionHandle(handle),
	}
}

func resourceSlackUserGroupUpdate(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	client := m.(*providerClient)

	id := d.Id()
	name := d.Get("name").(string)
	description := d.Get("description").(string)
	handle := d.Get("handle").(string)
	channels := d.Get("channels").(*schema.Set)
	users := d.Get("users").(*schema.Set)

	_, err := client.UpdateUserGroupContext(ctx, id, userGroupUpdateOptions(name, description, handle, channels)...)
	if err != nil {
		return diag.Errorf("could not update usergroup %s: %s", name, err)
	}

	if d.HasChanges("users") {
		_, err := client.UpdateUserGroupMembersContext(ctx, id, strings.Join(schemaSetToSlice(users), ","))
		if err != nil {
			return diag.Errorf("could not update usergroup members %s: %s", name, err)
		}
		schemaSetToSlice(users)
	}
	return resourceSlackUserGroupRead(ctx, d, m)
}

func resourceSlackUserGroupDelete(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	var diags diag.Diagnostics
	client := m.(*providerClient)

	id := d.Id()
	_, err := client.DisableUserGroupContext(ctx, id)
	if err != nil && err.Error() != slackErrorAlreadyDisabled {
		return diag.FromErr(err)
	}

	return diags
}

func updateUserGroupData(d *schema.ResourceData, userGroup slack.UserGroup) diag.Diagnostics {
	if userGroup.ID == "" {
		return diag.Errorf("error setting id: returned usergroup does not have an id")
	}
	d.SetId(userGroup.ID)

	if err := d.Set("name", userGroup.Name); err != nil {
		return diag.Errorf("error setting name: %s", err)
	}

	if err := d.Set("handle", userGroup.Handle); err != nil {
		return diag.Errorf("error setting handle: %s", err)
	}

	if err := d.Set("description", userGroup.Description); err != nil {
		return diag.Errorf("error setting description: %s", err)
	}

	if err := d.Set("channels", userGroup.Prefs.Channels); err != nil {
		return diag.Errorf("error setting channels: %s", err)
	}

	if err := d.Set("users", userGroup.Users); err != nil {
		return diag.Errorf("error setting users: %s", err)
	}

	return nil
}
