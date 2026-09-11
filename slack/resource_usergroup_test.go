package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hashicorp/go-multierror"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/acctest"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
	"github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	userGroupResourceNamePrefix = "test-acc-slack-usergroup-test"
)

func init() {
	resource.AddTestSweepers("slack_usergroup", &resource.Sweeper{
		Name: "slack_useregroup",
		F: func(string) error {
			client, err := sharedSlackClient()
			if err != nil {
				return fmt.Errorf("error getting client: %s", err)
			}
			c := client.(*slack.Client)
			groups, err := c.GetUserGroupsContext(context.Background())
			if err != nil {
				return fmt.Errorf("[ERROR] error getting channels: %s", err)
			}
			var sweeperErrs *multierror.Error
			for _, group := range groups {
				if strings.HasPrefix(group.Name, userGroupResourceNamePrefix) {
					_, err := c.DisableUserGroupContext(context.Background(), group.ID)
					if err != nil {
						if err.Error() != "already_disabled" {
							sweeperErr := fmt.Errorf("disabling usergroup %s during sweep: %s", group.Name, err)
							log.Printf("[ERROR] %s", sweeperErr)
							sweeperErrs = multierror.Append(sweeperErrs, err)
						}
					} else {
						fmt.Printf("[INFO] disabled usergroup %s during sweep\n", group.Name)
					}
				}
			}
			return sweeperErrs.ErrorOrNil()
		},
	})
}

func TestAccSlackUserGroupTest(t *testing.T) {
	t.Parallel()

	resourceName := "slack_usergroup.test"

	t.Run("update name, description and handle", func(t *testing.T) {
		name := acctest.RandomWithPrefix(userGroupResourceNamePrefix)
		createUserGroup := testAccSlackUserGroup(name)

		updateName := acctest.RandomWithPrefix(userGroupResourceNamePrefix)
		updateUserGroup := testAccSlackUserGroup(updateName)

		testSlackUserGroupUpdate(t, resourceName, createUserGroup, &updateUserGroup)
	})

	t.Run("update users", func(t *testing.T) {
		name := acctest.RandomWithPrefix(userGroupResourceNamePrefix)
		createUserGroup := testAccSlackUserGroupWithUsers(name, []string{}, []string{testUser00.id, testUser01.id})

		updateUserGroup := createUserGroup
		updateUserGroup.Users = []string{testUser00.id}

		testSlackUserGroupUpdate(t, resourceName, createUserGroup, &updateUserGroup)
	})

	t.Run("update channels", func(t *testing.T) {
		channel := createTestConversation(t)

		name := acctest.RandomWithPrefix(userGroupResourceNamePrefix)
		createUserGroup := testAccSlackUserGroupWithUsers(name, []string{}, []string{})

		updateUserGroup := createUserGroup
		updateUserGroup.Prefs = slack.UserGroupPrefs{Channels: []string{channel.ID}}

		testSlackUserGroupUpdate(t, resourceName, createUserGroup, &updateUserGroup)
	})
}

func TestResourceSlackUserGroupCreateRestoresDisabledGroupByHandle(t *testing.T) {
	const (
		groupID       = "S123"
		desiredName   = "Platform incident subscribers"
		desiredHandle = "platform-incident-subscribers"
	)

	desiredDescription := "Users to auto-invite to incident channels for team Platform"
	desiredChannels := []string{"C123", "C456"}
	existingUsers := []string{"U123", "U456"}
	var enabled atomic.Bool
	var updated atomic.Bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, r.ParseForm())
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case "/usergroups.create":
			writeSlackTestResponse(t, w, map[string]interface{}{"ok": false, "error": "handle_already_exists"})
		case "/usergroups.list":
			assert.Equal(t, "true", r.Form.Get("include_disabled"))
			name := "Deprecated - Platform incident subscribers"
			dateDelete := int64(1720000000)
			if updated.Load() {
				name = desiredName
				dateDelete = 0
			}
			writeSlackTestResponse(t, w, map[string]interface{}{
				"ok": true,
				"usergroups": []map[string]interface{}{{
					"id":          groupID,
					"name":        name,
					"handle":      desiredHandle,
					"description": desiredDescription,
					"date_delete": dateDelete,
					"users":       existingUsers,
					"prefs":       map[string]interface{}{"channels": desiredChannels},
				}},
			})
		case "/usergroups.enable":
			assert.Equal(t, groupID, r.Form.Get("usergroup"))
			enabled.Store(true)
			writeSlackTestResponse(t, w, map[string]interface{}{"ok": true, "usergroup": map[string]interface{}{"id": groupID}})
		case "/usergroups.update":
			assert.True(t, enabled.Load(), "usergroup must be enabled before it is updated")
			assert.Equal(t, groupID, r.Form.Get("usergroup"))
			assert.Equal(t, desiredName, r.Form.Get("name"))
			assert.Equal(t, desiredHandle, r.Form.Get("handle"))
			assert.Equal(t, desiredDescription, r.Form.Get("description"))
			assert.ElementsMatch(t, desiredChannels, strings.Split(r.Form.Get("channels"), ","))
			updated.Store(true)
			writeSlackTestResponse(t, w, map[string]interface{}{"ok": true, "usergroup": map[string]interface{}{"id": groupID}})
		case "/usergroups.users.update":
			t.Error("members should be preserved when users is empty")
			writeSlackTestResponse(t, w, map[string]interface{}{"ok": false, "error": "unexpected_members_update"})
		default:
			t.Errorf("unexpected Slack API request: %s", r.URL.Path)
			writeSlackTestResponse(t, w, map[string]interface{}{"ok": false, "error": "unexpected_request"})
		}
	}))
	t.Cleanup(server.Close)

	resource := resourceSlackUserGroup()
	d := schema.TestResourceDataRaw(t, resource.Schema, map[string]interface{}{
		"name":        desiredName,
		"handle":      desiredHandle,
		"description": desiredDescription,
		"channels":    []interface{}{desiredChannels[0], desiredChannels[1]},
		"users":       []interface{}{},
	})
	client := slack.New("testing-token", slack.OptionAPIURL(server.URL+"/"))

	diagnostics := resourceSlackUserGroupCreate(context.Background(), d, client)
	require.False(t, diagnostics.HasError(), diagnostics)
	require.Equal(t, groupID, d.Id())
	require.True(t, enabled.Load())
	require.ElementsMatch(t, existingUsers, d.Get("users").(*schema.Set).List())
}

func TestResourceSlackUserGroupCreateRestoresDisabledGroupByName(t *testing.T) {
	const (
		groupID       = "S123"
		desiredName   = "Platform incident subscribers"
		desiredHandle = "platform-incident-subscribers"
		desiredUser   = "U123"
	)

	var enabled atomic.Bool
	var membersUpdated atomic.Bool
	var updated atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, r.ParseForm())
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case "/usergroups.create":
			writeSlackTestResponse(t, w, map[string]interface{}{"ok": false, "error": "name_already_exists"})
		case "/usergroups.list":
			assert.Equal(t, "true", r.Form.Get("include_disabled"))
			handle := "deprecated-platform-incident-subscribers"
			dateDelete := int64(1720000000)
			users := []string{}
			if updated.Load() {
				handle = desiredHandle
				dateDelete = 0
				users = []string{desiredUser}
			}
			writeSlackTestResponse(t, w, map[string]interface{}{
				"ok": true,
				"usergroups": []map[string]interface{}{{
					"id":          groupID,
					"name":        desiredName,
					"handle":      handle,
					"date_delete": dateDelete,
					"users":       users,
				}},
			})
		case "/usergroups.enable":
			enabled.Store(true)
			writeSlackTestResponse(t, w, map[string]interface{}{"ok": true, "usergroup": map[string]interface{}{"id": groupID}})
		case "/usergroups.update":
			assert.True(t, membersUpdated.Load(), "members must be updated before usergroup properties")
			updated.Store(true)
			writeSlackTestResponse(t, w, map[string]interface{}{"ok": true, "usergroup": map[string]interface{}{"id": groupID}})
		case "/usergroups.users.update":
			assert.True(t, enabled.Load(), "usergroup must be enabled before members are updated")
			assert.Equal(t, desiredUser, r.Form.Get("users"))
			membersUpdated.Store(true)
			writeSlackTestResponse(t, w, map[string]interface{}{"ok": true, "usergroup": map[string]interface{}{"id": groupID}})
		default:
			t.Errorf("unexpected Slack API request: %s", r.URL.Path)
			writeSlackTestResponse(t, w, map[string]interface{}{"ok": false, "error": "unexpected_request"})
		}
	}))
	t.Cleanup(server.Close)

	resource := resourceSlackUserGroup()
	d := schema.TestResourceDataRaw(t, resource.Schema, map[string]interface{}{
		"name":   desiredName,
		"handle": desiredHandle,
		"users":  []interface{}{desiredUser},
	})
	client := slack.New("testing-token", slack.OptionAPIURL(server.URL+"/"))

	diagnostics := resourceSlackUserGroupCreate(context.Background(), d, client)
	require.False(t, diagnostics.HasError(), diagnostics)
	require.Equal(t, groupID, d.Id())
	require.True(t, enabled.Load())
	require.Equal(t, desiredHandle, d.Get("handle"))
}

func TestResourceSlackUserGroupCreateRefusesActiveCollision(t *testing.T) {
	const (
		desiredName   = "Platform incident subscribers"
		desiredHandle = "platform-incident-subscribers"
	)

	tests := []struct {
		name               string
		collisionError     string
		existingName       string
		existingHandle     string
		expectedDiagnostic string
	}{
		{
			name:               "handle",
			collisionError:     "handle_already_exists",
			existingName:       "Another team",
			existingHandle:     desiredHandle,
			expectedDiagnostic: "handle platform-incident-subscribers is already used by active usergroup Another team (SACTIVE)",
		},
		{
			name:               "name",
			collisionError:     "name_already_exists",
			existingName:       desiredName,
			existingHandle:     "another-handle",
			expectedDiagnostic: "name Platform incident subscribers is already used by active usergroup Platform incident subscribers (SACTIVE)",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.NoError(t, r.ParseForm())
				w.Header().Set("Content-Type", "application/json")

				switch r.URL.Path {
				case "/usergroups.create":
					writeSlackTestResponse(t, w, map[string]interface{}{"ok": false, "error": test.collisionError})
				case "/usergroups.list":
					assert.Equal(t, "true", r.Form.Get("include_disabled"))
					writeSlackTestResponse(t, w, map[string]interface{}{
						"ok": true,
						"usergroups": []map[string]interface{}{{
							"id":          "SACTIVE",
							"name":        test.existingName,
							"handle":      test.existingHandle,
							"date_delete": 0,
						}},
					})
				default:
					t.Errorf("active usergroup must not be modified: %s", r.URL.Path)
					writeSlackTestResponse(t, w, map[string]interface{}{"ok": false, "error": "unexpected_mutation"})
				}
			}))
			t.Cleanup(server.Close)

			resource := resourceSlackUserGroup()
			d := schema.TestResourceDataRaw(t, resource.Schema, map[string]interface{}{
				"name":   desiredName,
				"handle": desiredHandle,
			})
			client := slack.New("testing-token", slack.OptionAPIURL(server.URL+"/"))

			diagnostics := resourceSlackUserGroupCreate(context.Background(), d, client)
			require.True(t, diagnostics.HasError())
			require.Contains(t, diagnostics[0].Summary, test.expectedDiagnostic)
			require.Empty(t, d.Id())
		})
	}
}

func TestResourceSlackUserGroupCreateRollsBackWhenUpdateFails(t *testing.T) {
	const (
		groupID = "S123"
		handle  = "platform-incident-subscribers"
	)

	var disabled atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, r.ParseForm())
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case "/usergroups.create":
			writeSlackTestResponse(t, w, map[string]interface{}{"ok": false, "error": "handle_already_exists"})
		case "/usergroups.list":
			assert.Equal(t, "true", r.Form.Get("include_disabled"))
			writeSlackTestResponse(t, w, map[string]interface{}{
				"ok": true,
				"usergroups": []map[string]interface{}{{
					"id":          groupID,
					"name":        "Deprecated - Platform incident subscribers",
					"handle":      handle,
					"date_delete": 1720000000,
				}},
			})
		case "/usergroups.update":
			writeSlackTestResponse(t, w, map[string]interface{}{"ok": false, "error": "internal_error"})
		case "/usergroups.enable":
			writeSlackTestResponse(t, w, map[string]interface{}{"ok": true, "usergroup": map[string]interface{}{"id": groupID}})
		case "/usergroups.disable":
			assert.Equal(t, groupID, r.Form.Get("usergroup"))
			disabled.Store(true)
			writeSlackTestResponse(t, w, map[string]interface{}{"ok": true, "usergroup": map[string]interface{}{"id": groupID}})
		default:
			t.Errorf("unexpected Slack API request: %s", r.URL.Path)
			writeSlackTestResponse(t, w, map[string]interface{}{"ok": false, "error": "unexpected_request"})
		}
	}))
	t.Cleanup(server.Close)

	resource := resourceSlackUserGroup()
	d := schema.TestResourceDataRaw(t, resource.Schema, map[string]interface{}{
		"name":   "Platform incident subscribers",
		"handle": handle,
	})
	client := slack.New("testing-token", slack.OptionAPIURL(server.URL+"/"))

	diagnostics := resourceSlackUserGroupCreate(context.Background(), d, client)
	require.True(t, diagnostics.HasError())
	require.Contains(t, diagnostics[0].Summary, "could not update restored usergroup")
	require.True(t, disabled.Load())
	require.Empty(t, d.Id())
}

func TestResourceSlackUserGroupCreateRollsBackWhenMemberUpdateFails(t *testing.T) {
	const (
		groupID = "S123"
		handle  = "platform-incident-subscribers"
	)

	var disabled atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, r.ParseForm())
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case "/usergroups.create":
			writeSlackTestResponse(t, w, map[string]interface{}{"ok": false, "error": "handle_already_exists"})
		case "/usergroups.list":
			writeSlackTestResponse(t, w, map[string]interface{}{
				"ok": true,
				"usergroups": []map[string]interface{}{{
					"id":          groupID,
					"name":        "Deprecated - Platform incident subscribers",
					"handle":      handle,
					"date_delete": 1720000000,
				}},
			})
		case "/usergroups.enable":
			writeSlackTestResponse(t, w, map[string]interface{}{"ok": true, "usergroup": map[string]interface{}{"id": groupID}})
		case "/usergroups.users.update":
			writeSlackTestResponse(t, w, map[string]interface{}{"ok": false, "error": "invalid_users"})
		case "/usergroups.disable":
			disabled.Store(true)
			writeSlackTestResponse(t, w, map[string]interface{}{"ok": true, "usergroup": map[string]interface{}{"id": groupID}})
		default:
			t.Errorf("unexpected Slack API request: %s", r.URL.Path)
			writeSlackTestResponse(t, w, map[string]interface{}{"ok": false, "error": "unexpected_request"})
		}
	}))
	t.Cleanup(server.Close)

	resource := resourceSlackUserGroup()
	d := schema.TestResourceDataRaw(t, resource.Schema, map[string]interface{}{
		"name":   "Platform incident subscribers",
		"handle": handle,
		"users":  []interface{}{"U123"},
	})
	client := slack.New("testing-token", slack.OptionAPIURL(server.URL+"/"))

	diagnostics := resourceSlackUserGroupCreate(context.Background(), d, client)
	require.True(t, diagnostics.HasError())
	require.Contains(t, diagnostics[0].Summary, "could not update restored usergroup members")
	require.True(t, disabled.Load())
	require.Empty(t, d.Id())
}

func TestRollbackRestoredUserGroupKeepsIDWhenDisableFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/usergroups.disable", r.URL.Path)
		writeSlackTestResponse(t, w, map[string]interface{}{"ok": false, "error": "internal_error"})
	}))
	t.Cleanup(server.Close)

	d := resourceSlackUserGroup().TestResourceData()
	d.SetId("S123")
	client := slack.New("testing-token", slack.OptionAPIURL(server.URL+"/"))

	diagnostics := rollbackRestoredUserGroup(context.Background(), d, client, fmt.Errorf("update failed"))
	require.True(t, diagnostics.HasError())
	require.Contains(t, diagnostics[0].Summary, "update failed; could not disable usergroup S123 again: internal_error")
	require.Equal(t, "S123", d.Id())
}

func TestRollbackRestoredUserGroupClearsIDWhenAlreadyDisabled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/usergroups.disable", r.URL.Path)
		writeSlackTestResponse(t, w, map[string]interface{}{"ok": false, "error": "already_disabled"})
	}))
	t.Cleanup(server.Close)

	d := resourceSlackUserGroup().TestResourceData()
	d.SetId("S123")
	client := slack.New("testing-token", slack.OptionAPIURL(server.URL+"/"))

	diagnostics := rollbackRestoredUserGroup(context.Background(), d, client, fmt.Errorf("update failed"))
	require.True(t, diagnostics.HasError())
	require.Contains(t, diagnostics[0].Summary, "update failed")
	require.Empty(t, d.Id())
}

func TestResourceSlackUserGroupCreateHandlesRestoreEnableFailure(t *testing.T) {
	const handle = "platform-incident-subscribers"

	tests := []struct {
		enableError        string
		expectedDiagnostic string
		expectedID         string
	}{
		{enableError: "internal_error", expectedDiagnostic: "could not enable usergroup", expectedID: "S123"},
		{enableError: "already_enabled", expectedDiagnostic: "usergroup became active before it could be restored", expectedID: ""},
	}

	for _, test := range tests {
		t.Run(test.enableError, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.NoError(t, r.ParseForm())
				w.Header().Set("Content-Type", "application/json")

				switch r.URL.Path {
				case "/usergroups.create":
					writeSlackTestResponse(t, w, map[string]interface{}{"ok": false, "error": "handle_already_exists"})
				case "/usergroups.list":
					writeSlackTestResponse(t, w, map[string]interface{}{
						"ok": true,
						"usergroups": []map[string]interface{}{{
							"id":          "S123",
							"name":        "Deprecated - Platform incident subscribers",
							"handle":      handle,
							"date_delete": 1720000000,
						}},
					})
				case "/usergroups.enable":
					writeSlackTestResponse(t, w, map[string]interface{}{"ok": false, "error": test.enableError})
				default:
					t.Errorf("usergroup must not be updated after enable fails: %s", r.URL.Path)
					writeSlackTestResponse(t, w, map[string]interface{}{"ok": false, "error": "unexpected_request"})
				}
			}))
			t.Cleanup(server.Close)

			resource := resourceSlackUserGroup()
			d := schema.TestResourceDataRaw(t, resource.Schema, map[string]interface{}{
				"name":   "Platform incident subscribers",
				"handle": handle,
			})
			client := slack.New("testing-token", slack.OptionAPIURL(server.URL+"/"))

			diagnostics := resourceSlackUserGroupCreate(context.Background(), d, client)
			require.True(t, diagnostics.HasError())
			require.Contains(t, diagnostics[0].Summary, test.expectedDiagnostic)
			require.Equal(t, test.expectedID, d.Id())
		})
	}
}

func TestResourceSlackUserGroupDeleteAcceptsAlreadyDisabled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, r.ParseForm())
		assert.Equal(t, "/usergroups.disable", r.URL.Path)
		writeSlackTestResponse(t, w, map[string]interface{}{"ok": false, "error": "already_disabled"})
	}))
	t.Cleanup(server.Close)

	d := resourceSlackUserGroup().TestResourceData()
	d.SetId("S123")
	client := slack.New("testing-token", slack.OptionAPIURL(server.URL+"/"))

	diagnostics := resourceSlackUserGroupDelete(context.Background(), d, client)
	require.False(t, diagnostics.HasError(), diagnostics)
}

func writeSlackTestResponse(t *testing.T, w http.ResponseWriter, response interface{}) {
	t.Helper()
	assert.NoError(t, json.NewEncoder(w).Encode(response))
}

func createTestConversation(t *testing.T) *slack.Channel {
	client, err := sharedSlackClient()
	if err != nil {
		require.NoError(t, err, "error getting client: %s", err)
	}

	c := client.(*slack.Client)
	channelName := acctest.RandomWithPrefix(conversationNamePrefix)

	channel, err := c.CreateConversationContext(context.Background(), slack.CreateConversationParams{
		ChannelName: channelName,
		IsPrivate:   false,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = c.ArchiveConversationContext(context.Background(), channel.ID)
	})
	return channel
}

func testSlackUserGroupUpdate(t *testing.T, resourceName string, createChannel slack.UserGroup, updateChannel *slack.UserGroup) {
	var providers []*schema.Provider
	steps := []resource.TestStep{
		{
			Config: testAccSlackUserGroupConfig(createChannel),
			Check: resource.ComposeTestCheckFunc(
				testCheckSlackUserGroupAttributes(t, resourceName, createChannel),
				testCheckUserGroupAttrBasic(resourceName, createChannel),
			),
		},
		{
			ResourceName:      resourceName,
			ImportState:       true,
			ImportStateVerify: true,
		},
	}

	if updateChannel != nil {
		steps = append(steps, resource.TestStep{
			Config: testAccSlackUserGroupConfig(*updateChannel),
			Check: resource.ComposeTestCheckFunc(
				testCheckSlackUserGroupAttributes(t, resourceName, *updateChannel),
				testCheckUserGroupAttrBasic(resourceName, *updateChannel),
			),
		},
		)
	}

	resource.ParallelTest(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
		},
		IDRefreshName:     resourceName,
		ProviderFactories: testAccProviderFactories(&providers),
		CheckDestroy:      testAccCheckUserGroupDestroy,
		Steps:             steps,
	})
}

func testCheckUserGroupAttrBasic(resourceName string, channel slack.UserGroup) resource.TestCheckFunc {
	return resource.ComposeTestCheckFunc(
		resource.TestCheckResourceAttr(resourceName, "name", channel.Name),
		resource.TestCheckResourceAttr(resourceName, "description", channel.Description),
		resource.TestCheckResourceAttr(resourceName, "handle", channel.Handle),

		testCheckResourceAttrSlice(resourceName, "users", channel.Users),
		testCheckResourceAttrSlice(resourceName, "channels", channel.Prefs.Channels),
	)
}

func testCheckSlackUserGroupAttributes(t *testing.T, resourceName string, expectedGroup slack.UserGroup) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[resourceName]
		if !ok {
			return fmt.Errorf("not found: %s", resourceName)
		}

		primary := rs.Primary
		group, err := findUserGroupByID(
			context.Background(),
			resourceSlackUserGroup().TestResourceData(),
			testAccProvider.Meta(),
			primary.ID,
			false,
		)
		if err != nil {
			return fmt.Errorf("couldn't get conversation info for %s: %s", primary.ID, err)
		}

		require.Equal(t, primary.Attributes["name"], group.Name, "usergroup name does not match")
		require.Equal(t, primary.Attributes["description"], group.Description, "usergroup description does not match")
		require.Equal(t, primary.Attributes["handle"], group.Handle, "usergroup handle does not match")

		usersLength, _ := strconv.Atoi(primary.Attributes["users.#"])
		require.Equal(t, len(expectedGroup.Users), usersLength, "defined users length should match state")
		require.Equal(t, len(expectedGroup.Users), len(group.Users), "defined users length should match users in channel")

		for i := 0; i < usersLength; i++ {
			user := primary.Attributes[fmt.Sprintf("users.%d", i)]
			require.True(t, contains(group.Users, user), "user should be in the group")
			require.True(t, contains(expectedGroup.Users, user), "user in state should be defined in the resource")
		}

		channelsLength, _ := strconv.Atoi(primary.Attributes["channels.#"])
		require.Equal(t, len(expectedGroup.Prefs.Channels), channelsLength, "defined channels length should match state")
		require.Equal(t, len(expectedGroup.Prefs.Channels), len(group.Prefs.Channels), "defined channels length should match users in channel")

		for i := 0; i < channelsLength; i++ {
			channel := primary.Attributes[fmt.Sprintf("channels.%d", i)]
			require.True(t, contains(group.Prefs.Channels, channel), "channel should be in the group")
			require.True(t, contains(expectedGroup.Prefs.Channels, channel), "channel in state should be defined in the resource")
		}

		return nil
	}
}

func testAccSlackUserGroup(name string) slack.UserGroup {
	return testAccSlackUserGroupWithUsers(name, []string{}, []string{})
}

func testAccSlackUserGroupWithUsers(name string, channels, users []string) slack.UserGroup {
	sort.Strings(users)
	group := slack.UserGroup{
		Name:        name,
		Description: fmt.Sprintf("Description for %s", name),
		Handle:      fmt.Sprintf("handle-for-%s", name),
		Users:       users,
		Prefs:       slack.UserGroupPrefs{Channels: channels},
	}
	return group
}

func testAccCheckUserGroupDestroy(s *terraform.State) error {
	c := testAccProvider.Meta().(*slack.Client)
	for _, rs := range s.RootModule().Resources {
		if rs.Type != "slack_usergroup" {
			continue
		}

		_, err := c.DisableUserGroupContext(context.Background(), rs.Primary.ID)
		if err != nil && err.Error() != "already_disabled" {
			return fmt.Errorf("error disabling usergroup %s: %s", rs.Primary.ID, err)
		}
	}

	return nil
}

func testAccSlackUserGroupConfig(c slack.UserGroup) string {
	var channels, users []string
	for _, channel := range c.Prefs.Channels {
		channels = append(channels, fmt.Sprintf(`"%s"`, channel))
	}
	for _, user := range c.Users {
		users = append(users, fmt.Sprintf(`"%s"`, user))
	}

	return fmt.Sprintf(`
resource slack_usergroup test {
  name        = "%s"
  description = "%s"
  handle      = "%s"
  users       = [%s]
  channels    =  [%s]
}
`, c.Name, c.Description, c.Handle, strings.Join(users, ","), strings.Join(channels, ","))
}
