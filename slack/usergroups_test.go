package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/slack-go/slack"
	"github.com/stretchr/testify/require"
)

const (
	trueString               = "true"
	userGroupsUpdateEndpoint = "usergroups.update"
)

func TestUserGroupRefreshCache(t *testing.T) {
	var requests, renamed int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		if r.URL.Path != "/usergroups.list" || r.FormValue("include_users") != trueString {
			t.Errorf("unexpected request: %s %s", r.URL.Path, r.Form)
		}
		name := "Active"
		if atomic.LoadInt32(&renamed) != 0 {
			name = "Renamed"
		}
		groups := fmt.Sprintf(`{"id":"S1","name":%q,"handle":"active","users":["U1"],"prefs":{"channels":["C1"]}}`, name)
		if r.FormValue("include_disabled") == trueString {
			groups += `,{"id":"S2","name":"Disabled","handle":"disabled","date_delete":1}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"ok":true,"usergroups":[%s]}`, groups)
	}))
	defer server.Close()
	client := testUserGroupClient(t, server)
	resource := resourceSlackUserGroup()

	var readers sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 231; i++ {
		d := schema.TestResourceDataRaw(t, resource.Schema, nil)
		id, name := "S1", "Active"
		if i%2 != 0 {
			id, name = "S2", "Disabled"
		}
		d.SetId(id)
		readers.Add(1)
		go func() {
			defer readers.Done()
			<-start
			if diagnostics := resource.ReadContext(context.Background(), d, client); diagnostics.HasError() {
				t.Error(diagnostics)
				return
			}
			if d.Id() != id || d.Get("name") != name {
				t.Errorf("read %s: got ID %q and name %q", id, d.Id(), d.Get("name"))
			}
		}()
	}
	close(start)
	readers.Wait()
	require.EqualValues(t, 1, atomic.LoadInt32(&requests), "resource refreshes must share one list request")

	for _, query := range []map[string]interface{}{{"name": "Active"}, {"usergroup_id": "S1"}} {
		d := schema.TestResourceDataRaw(t, dataSourceUserGroup().Schema, query)
		require.False(t, dataSourceUserGroupRead(context.Background(), d, client).HasError())
		require.Equal(t, "S1", d.Id())
		require.Equal(t, "active", d.Get("handle"))
		require.Equal(t, []interface{}{"U1"}, d.Get("users").(*schema.Set).List())
		require.Equal(t, []interface{}{"C1"}, d.Get("channels").(*schema.Set).List())
	}
	d := schema.TestResourceDataRaw(t, dataSourceUserGroup().Schema, map[string]interface{}{"name": "Disabled"})
	require.True(t, dataSourceUserGroupRead(context.Background(), d, client).HasError())
	d = schema.TestResourceDataRaw(t, resource.Schema, nil)
	d.SetId("S2")
	require.False(t, resource.ReadContext(context.Background(), d, client).HasError())
	require.Equal(t, "S2", d.Id(), "active-only lookups must not replace the cache that includes disabled groups")
	require.EqualValues(t, 2, atomic.LoadInt32(&requests), "data sources must share a separate active-only list")

	atomic.StoreInt32(&renamed, 1)
	d = schema.TestResourceDataRaw(t, resource.Schema, nil)
	d.SetId("S1")
	require.False(t, resource.ReadContext(context.Background(), d, testUserGroupClient(t, server)).HasError())
	require.Equal(t, "Renamed", d.Get("name"), "a new provider configuration must fetch fresh data")
	require.EqualValues(t, 3, atomic.LoadInt32(&requests))
}

func TestUserGroupCacheInvalidation(t *testing.T) {
	tests := []struct {
		endpoint string
		mutate   func(*providerClient) (slack.UserGroup, error)
	}{
		{"usergroups.create", func(c *providerClient) (slack.UserGroup, error) {
			return c.CreateUserGroupContext(context.Background(), slack.UserGroup{Name: "Target"})
		}},
		{"usergroups.enable", func(c *providerClient) (slack.UserGroup, error) {
			return c.EnableUserGroupContext(context.Background(), "S1")
		}},
		{userGroupsUpdateEndpoint, func(c *providerClient) (slack.UserGroup, error) {
			return c.UpdateUserGroupContext(context.Background(), "S1", slack.UpdateUserGroupsOptionName("Renamed"))
		}},
		{"usergroups.users.update", func(c *providerClient) (slack.UserGroup, error) {
			return c.UpdateUserGroupMembersContext(context.Background(), "S1", "U2")
		}},
		{"usergroups.disable", func(c *providerClient) (slack.UserGroup, error) {
			return c.DisableUserGroupContext(context.Background(), "S1")
		}},
	}
	for _, test := range tests {
		t.Run(test.endpoint, func(t *testing.T) {
			target := slack.UserGroup{ID: "S1", Name: "Target", Users: []string{"U1"}}
			stable := slack.UserGroup{ID: "S2", Name: "Unaffected"}
			before, after := []slack.UserGroup{target, stable}, []slack.UserGroup{target, stable}
			switch test.endpoint {
			case "usergroups.create":
				before = []slack.UserGroup{stable}
			case "usergroups.enable":
				before[0].DateDelete = 1
			case userGroupsUpdateEndpoint:
				after[0].Name = "Renamed"
			case "usergroups.users.update":
				after[0].Users = []string{"U2"}
			case "usergroups.disable":
				after[0].DateDelete = 1
			}
			var mutated, requests int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path != "/usergroups.list" {
					if r.URL.Path != "/"+test.endpoint {
						t.Errorf("unexpected mutation: %s", r.URL.Path)
					}
					atomic.StoreInt32(&mutated, 1)
					if test.endpoint == userGroupsUpdateEndpoint {
						_, _ = fmt.Fprint(w, `{"ok":false,"error":"internal_error"}`)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "usergroup": after[0]})
					return
				}
				atomic.AddInt32(&requests, 1)
				groups := before
				if atomic.LoadInt32(&mutated) != 0 {
					groups = after
				}
				visible := []slack.UserGroup{}
				for _, group := range groups {
					if r.FormValue("include_disabled") == trueString || group.DateDelete == 0 {
						visible = append(visible, group)
					}
				}
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "usergroups": visible})
			}))
			defer server.Close()
			client := testUserGroupClient(t, server)
			d := resourceSlackUserGroup().TestResourceData()
			for _, includeDisabled := range []bool{true, false} {
				_, err := ListUserGroupsWithContext(context.Background(), d, client, includeDisabled)
				require.NoError(t, err)
			}
			_, err := test.mutate(client)
			if test.endpoint == userGroupsUpdateEndpoint {
				require.Error(t, err, "a failed response can follow a remote mutation")
			} else {
				require.NoError(t, err)
			}
			all, err := ListUserGroupsWithContext(context.Background(), d, client, true)
			require.NoError(t, err)
			require.Equal(t, after, all)
			active, err := ListUserGroupsWithContext(context.Background(), d, client, false)
			require.NoError(t, err)
			if test.endpoint == "usergroups.disable" {
				require.Equal(t, []slack.UserGroup{stable}, active)
			} else {
				require.Equal(t, after, active)
			}
			require.EqualValues(t, 4, atomic.LoadInt32(&requests), "a mutation must invalidate both list variants")
		})
	}
}

func TestUserGroupCacheDoesNotRetainFailures(t *testing.T) {
	var requests int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if atomic.AddInt32(&requests, 1) == 1 {
			_, _ = fmt.Fprint(w, `{"ok":false,"error":"internal_error"}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"ok":true,"usergroups":[]}`)
	}))
	defer server.Close()
	client := testUserGroupClient(t, server)
	resource := resourceSlackUserGroup()
	d := resource.TestResourceData()
	d.SetId("S1")
	require.True(t, resource.ReadContext(context.Background(), d, client).HasError())
	require.Equal(t, "S1", d.Id(), "a failed list request must not remove the resource from state")
	require.False(t, resource.ReadContext(context.Background(), d, client).HasError())
	require.Empty(t, d.Id())
	d.SetId("S2")
	require.False(t, resource.ReadContext(context.Background(), d, client).HasError())
	require.Empty(t, d.Id())
	require.EqualValues(t, 2, atomic.LoadInt32(&requests), "a successful empty list must be cached")
}

func TestUserGroupCacheInvalidationDuringRead(t *testing.T) {
	for _, failFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("first_request_fails=%t", failFirst), func(t *testing.T) {
			var requests int32
			started, release := make(chan struct{}), make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/usergroups.update" {
					_, _ = fmt.Fprint(w, `{"ok":true,"usergroup":{"id":"S1","name":"After"}}`)
					return
				}
				name := "After"
				if atomic.AddInt32(&requests, 1) == 1 {
					close(started)
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
					if failFirst {
						_, _ = fmt.Fprint(w, `{"ok":false,"error":"internal_error"}`)
						return
					}
					name = "Before"
				}
				_, _ = fmt.Fprintf(w, `{"ok":true,"usergroups":[{"id":"S1","name":%q}]}`, name)
			}))
			defer server.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			client := testUserGroupClient(t, server)
			d := resourceSlackUserGroup().TestResourceData()
			d.SetId("S1")
			done := make(chan bool, 1)
			go func() {
				done <- resourceSlackUserGroupRead(ctx, d, client).HasError()
			}()
			<-started
			canceled, cancelWaiter := context.WithCancel(ctx)
			cancelWaiter()
			_, err := ListUserGroupsWithContext(canceled, resourceSlackUserGroup().TestResourceData(), client, true)
			require.ErrorIs(t, err, context.Canceled)

			updated := resourceSlackUserGroup().TestResourceData()
			updated.SetId("S1")
			require.NoError(t, updated.Set("name", "After"))
			require.False(t, resourceSlackUserGroupUpdate(ctx, updated, client).HasError())
			require.Equal(t, "After", updated.Get("name"))
			close(release)
			require.Equal(t, failFirst, <-done)
			require.False(t, resourceSlackUserGroupRead(ctx, updated, client).HasError())
			require.Equal(t, "After", updated.Get("name"), "an old request must not replace a newer snapshot")
			require.EqualValues(t, 2, atomic.LoadInt32(&requests))
		})
	}
}

func testUserGroupClient(t *testing.T, server *httptest.Server) *providerClient {
	t.Helper()
	d := schema.TestResourceDataRaw(t, Provider().Schema, map[string]interface{}{"token": "test-token"})
	meta, diagnostics := providerConfigure(context.Background(), d)
	require.False(t, diagnostics.HasError())
	client := meta.(*providerClient)
	client.Client = slack.New("test-token", slack.OptionAPIURL(server.URL+"/"), slack.OptionHTTPClient(server.Client()))
	return client
}
