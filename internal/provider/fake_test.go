package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mac-lucky/terraform-provider-pushward/internal/client"
)

// fakeAPI is an in-memory org API with the server's problem shapes. It
// takes any hlo_ bearer token as a manage key of its one organization.
type fakeAPI struct {
	t   *testing.T
	srv *httptest.Server

	mu        sync.Mutex
	n         int
	org       client.Org
	extraOrgs int
	// orgGone answers every /orgs/{org_id} route with 404 org.not_found, as
	// the server does for a key whose organization it cannot find.
	orgGone  bool
	members  []*client.Member
	invites  map[string]*client.Invite
	groups   map[string]*client.Group
	tags     map[string]*client.Tag
	routes   []*client.Route
	keys     map[string]*client.Key
	devices  []client.Device
	pageSize int
	requests []string
}

func newFake(t *testing.T) *fakeAPI {
	f := &fakeAPI{
		t:        t,
		org:      client.Org{ID: "0a000000-0000-4000-8000-000000000001", Name: "Acme", Slug: "acme", Role: "admin", Plan: "team", Status: "active", MemberLimit: 50, CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)},
		invites:  map[string]*client.Invite{},
		groups:   map[string]*client.Group{},
		tags:     map[string]*client.Tag{},
		keys:     map[string]*client.Key{},
		pageSize: 2,
	}
	owner := "Owner"
	f.members = []*client.Member{{UserID: "0b000000-0000-4000-8000-000000000001", Nickname: &owner, Role: "owner", JoinedAt: f.org.CreatedAt}}
	mux := http.NewServeMux()
	f.routesFor(mux)
	f.srv = httptest.NewServer(f.auth(mux))
	t.Cleanup(f.srv.Close)
	return f
}

// config is the provider block for f followed by body.
func (f *fakeAPI) config(body string) string {
	return fmt.Sprintf("provider \"pushward\" {\n  api_key = \"hlo_test\"\n  api_url = %q\n}\n\n%s", f.srv.URL, body)
}

func (f *fakeAPI) id() string {
	f.n++
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", f.n)
}

func problem(w http.ResponseWriter, status int, code, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	p := map[string]any{"title": http.StatusText(status), "status": status, "detail": detail, "instance": "/requests/fake"}
	if code != "" {
		p["code"] = code
	}
	_ = json.NewEncoder(w).Encode(p)
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeAPI) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer hlo_") {
			problem(w, 401, "token.invalid", "invalid token")
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)
		if parts := strings.Split(r.URL.Path, "/"); len(parts) > 2 && (parts[2] != f.org.ID || f.orgGone) {
			problem(w, 404, "org.not_found", "organization not found")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		problem(w, 400, "", err.Error())
		return false
	}
	return true
}

// count reports how many requests matched method and a path suffix.
func (f *fakeAPI) count(method, suffix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if strings.HasPrefix(r, method+" ") && strings.HasSuffix(r, suffix) {
			n++
		}
	}
	return n
}

func (f *fakeAPI) member(id string) *client.Member {
	for _, m := range f.members {
		if m.UserID == id {
			return m
		}
	}
	return nil
}

// usable reports whether someone can still join with the invite: not
// revoked, not expired and not used up.
func usable(inv *client.Invite) bool {
	return inv.RevokedAt == nil && time.Now().Before(inv.ExpiresAt) && inv.Uses < inv.MaxUses
}

// lowerIDs lowercases and dedupes an id list, as the API does with the ids it
// is sent.
func lowerIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id = strings.ToLower(id); !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

func (f *fakeAPI) emailTaken(email string) bool {
	for _, m := range f.members {
		if m.Email != nil && strings.EqualFold(*m.Email, email) {
			return true
		}
	}
	for _, inv := range f.invites {
		if inv.Email != nil && strings.EqualFold(*inv.Email, email) && usable(inv) {
			return true
		}
	}
	return false
}

// join redeems an open invite: a new member with the invite's role and its
// email as their label.
func (f *fakeAPI) join(inviteID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	inv := f.invites[inviteID]
	if inv == nil || inv.RedeemedBy != nil {
		f.t.Fatalf("join: no open invite %s", inviteID)
	}
	uid := f.id()
	email := *inv.Email
	f.members = append(f.members, &client.Member{UserID: uid, Role: inv.Role, Email: &email, JoinedAt: time.Now().UTC()})
	inv.RedeemedBy, inv.Uses = &uid, 1
	return uid
}

func (f *fakeAPI) expire(inviteID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invites[inviteID].ExpiresAt = time.Now().Add(-time.Minute)
}

// useUp leaves an invite as it is once the person who joined with it deleted
// their account: no uses left and no redeemed_by.
func (f *fakeAPI) useUp(inviteID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invites[inviteID].Uses = f.invites[inviteID].MaxUses
}

func (f *fakeAPI) addMember(role, email string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	uid := f.id()
	m := &client.Member{UserID: uid, Role: role, JoinedAt: time.Now().UTC()}
	if email != "" {
		m.Email = &email
	}
	f.members = append(f.members, m)
	return uid
}

func (f *fakeAPI) removeMember(uid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.members = slices.DeleteFunc(f.members, func(m *client.Member) bool { return m.UserID == uid })
}

func (f *fakeAPI) memberRole(uid string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m := f.member(uid); m != nil {
		return m.Role
	}
	return ""
}

func (f *fakeAPI) inviteRevoked(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.invites[id] != nil && f.invites[id].RevokedAt != nil
}

// drop deletes a group, tag, route or key behind Terraform's back.
func (f *fakeAPI) drop(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.groups, id)
	delete(f.tags, id)
	delete(f.keys, id)
	f.routes = slices.DeleteFunc(f.routes, func(r *client.Route) bool { return r.ID == id })
}

func (f *fakeAPI) routeOrder() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []string
	for _, r := range f.routes {
		ids = append(ids, r.ID)
	}
	return ids
}

func (f *fakeAPI) routesFor(mux *http.ServeMux) {
	mux.HandleFunc("GET /orgs", func(w http.ResponseWriter, r *http.Request) {
		items := []client.Org{f.org}
		for range f.extraOrgs {
			items = append(items, f.org)
		}
		reply(w, 200, map[string]any{"items": items})
	})
	mux.HandleFunc("GET /orgs/{org}", func(w http.ResponseWriter, r *http.Request) {
		o := f.org
		o.MemberCount = int64(len(f.members))
		reply(w, 200, o)
	})
	mux.HandleFunc("GET /orgs/{org}/devices", func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, map[string]any{"items": f.devices})
	})

	// Members, paged by index.
	mux.HandleFunc("GET /orgs/{org}/members", func(w http.ResponseWriter, r *http.Request) {
		start := 0
		if c := r.URL.Query().Get("cursor"); c != "" {
			var err error
			if start, err = strconv.Atoi(c); err != nil {
				problem(w, 400, "cursor.invalid", "malformed cursor")
				return
			}
		}
		end := min(start+f.pageSize, len(f.members))
		out := map[string]any{"items": f.members[start:end]}
		if end < len(f.members) {
			out["next_cursor"] = strconv.Itoa(end)
		}
		reply(w, 200, out)
	})
	mux.HandleFunc("GET /orgs/{org}/members/{id}", func(w http.ResponseWriter, r *http.Request) {
		m := f.member(r.PathValue("id"))
		if m == nil {
			problem(w, 404, client.CodeMemberNotFound, "member not found")
			return
		}
		reply(w, 200, m)
	})
	mux.HandleFunc("PATCH /orgs/{org}/members/{id}", func(w http.ResponseWriter, r *http.Request) {
		m := f.member(r.PathValue("id"))
		if m == nil {
			problem(w, 404, client.CodeMemberNotFound, "member not found")
			return
		}
		var body map[string]json.RawMessage
		if !decode(w, r, &body) {
			return
		}
		if raw, ok := body["role"]; ok {
			var role string
			_ = json.Unmarshal(raw, &role)
			if (role != "member" && role != "viewer") || (m.Role != "member" && m.Role != "viewer") {
				problem(w, 403, "org.permission_denied", "your role in this organization does not allow this")
				return
			}
			m.Role = role
		}
		if raw, ok := body["email"]; ok {
			var email *string
			_ = json.Unmarshal(raw, &email)
			if email != nil {
				e := strings.ToLower(strings.TrimSpace(*email))
				email = &e
			}
			m.Email = email
		}
		reply(w, 200, m)
	})
	mux.HandleFunc("DELETE /orgs/{org}/members/{id}", func(w http.ResponseWriter, r *http.Request) {
		if f.member(r.PathValue("id")) == nil {
			problem(w, 404, client.CodeMemberNotFound, "member not found")
			return
		}
		f.members = slices.DeleteFunc(f.members, func(m *client.Member) bool { return m.UserID == r.PathValue("id") })
		w.WriteHeader(204)
	})

	// Invites.
	mux.HandleFunc("POST /orgs/{org}/invites", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Role  string  `json:"role"`
			Email *string `json:"email"`
		}
		if !decode(w, r, &body) {
			return
		}
		if body.Role != "member" && body.Role != "viewer" {
			problem(w, 403, "org.permission_denied", "your role in this organization does not allow this")
			return
		}
		if body.Email == nil || !strings.Contains(*body.Email, "@") {
			problem(w, 422, "email.invalid", "email must be a valid email address")
			return
		}
		email := strings.ToLower(strings.TrimSpace(*body.Email))
		if f.emailTaken(email) {
			problem(w, 409, client.CodeMemberEmailTaken, "a member or open invite of this organization already has this email")
			return
		}
		inv := &client.Invite{ID: f.id(), Role: body.Role, Email: &email, MaxUses: 1, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(168 * time.Hour).UTC()}
		f.invites[inv.ID] = inv
		reply(w, 201, inv)
	})
	mux.HandleFunc("GET /orgs/{org}/invites/{id}", func(w http.ResponseWriter, r *http.Request) {
		inv := f.invites[r.PathValue("id")]
		if inv == nil {
			problem(w, 404, client.CodeInviteNotFound, "invite not found")
			return
		}
		reply(w, 200, inv)
	})
	mux.HandleFunc("DELETE /orgs/{org}/invites/{id}", func(w http.ResponseWriter, r *http.Request) {
		inv := f.invites[r.PathValue("id")]
		if inv == nil {
			problem(w, 404, client.CodeInviteNotFound, "invite not found")
			return
		}
		now := time.Now().UTC()
		inv.RevokedAt = &now
		w.WriteHeader(204)
	})

	// Groups and tags.
	type named struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
	}
	nameTaken := func(name string, self string) bool {
		for id, g := range f.groups {
			if g.Name == name && id != self {
				return true
			}
		}
		return false
	}
	// desc trims, and stores a blank description as none.
	desc := func(d *string) *string {
		if d == nil || strings.TrimSpace(*d) == "" {
			return nil
		}
		v := strings.TrimSpace(*d)
		return &v
	}
	mux.HandleFunc("POST /orgs/{org}/groups", func(w http.ResponseWriter, r *http.Request) {
		var body named
		if !decode(w, r, &body) {
			return
		}
		if body.Name == nil || nameTaken(*body.Name, "") {
			problem(w, 409, "org.name_taken", "that name is already used in this organization")
			return
		}
		g := &client.Group{ID: f.id(), Name: *body.Name, Description: desc(body.Description), MemberIDs: []string{}, CreatedAt: time.Now().UTC()}
		f.groups[g.ID] = g
		reply(w, 201, g)
	})
	mux.HandleFunc("GET /orgs/{org}/groups/{id}", func(w http.ResponseWriter, r *http.Request) {
		g := f.groups[r.PathValue("id")]
		if g == nil {
			problem(w, 404, client.CodeGroupNotFound, "group not found")
			return
		}
		reply(w, 200, g)
	})
	mux.HandleFunc("PATCH /orgs/{org}/groups/{id}", func(w http.ResponseWriter, r *http.Request) {
		g := f.groups[r.PathValue("id")]
		if g == nil {
			problem(w, 404, client.CodeGroupNotFound, "group not found")
			return
		}
		var body named
		if !decode(w, r, &body) {
			return
		}
		if body.Name != nil {
			if nameTaken(*body.Name, g.ID) {
				problem(w, 409, "org.name_taken", "that name is already used in this organization")
				return
			}
			g.Name = *body.Name
		}
		if body.Description != nil {
			g.Description = desc(body.Description)
		}
		reply(w, 200, g)
	})
	mux.HandleFunc("PUT /orgs/{org}/groups/{id}/members", func(w http.ResponseWriter, r *http.Request) {
		g := f.groups[r.PathValue("id")]
		if g == nil {
			problem(w, 404, client.CodeGroupNotFound, "group not found")
			return
		}
		var body struct {
			UserIDs []string `json:"user_ids"`
		}
		if !decode(w, r, &body) {
			return
		}
		body.UserIDs = lowerIDs(body.UserIDs)
		for _, id := range body.UserIDs {
			if f.member(id) == nil {
				problem(w, 422, "org.routing_invalid", "the change names a member, device, group or tag that is not in this organization")
				return
			}
		}
		g.MemberIDs = body.UserIDs
		reply(w, 200, g)
	})
	mux.HandleFunc("DELETE /orgs/{org}/groups/{id}", func(w http.ResponseWriter, r *http.Request) {
		if f.groups[r.PathValue("id")] == nil {
			problem(w, 404, client.CodeGroupNotFound, "group not found")
			return
		}
		delete(f.groups, r.PathValue("id"))
		w.WriteHeader(204)
	})
	mux.HandleFunc("POST /orgs/{org}/tags", func(w http.ResponseWriter, r *http.Request) {
		var body named
		if !decode(w, r, &body) {
			return
		}
		t := &client.Tag{ID: f.id(), Name: *body.Name, Description: desc(body.Description), CreatedAt: time.Now().UTC()}
		f.tags[t.ID] = t
		reply(w, 201, t)
	})
	mux.HandleFunc("GET /orgs/{org}/tags/{id}", func(w http.ResponseWriter, r *http.Request) {
		t := f.tags[r.PathValue("id")]
		if t == nil {
			problem(w, 404, client.CodeTagNotFound, "tag not found")
			return
		}
		reply(w, 200, t)
	})
	mux.HandleFunc("PATCH /orgs/{org}/tags/{id}", func(w http.ResponseWriter, r *http.Request) {
		t := f.tags[r.PathValue("id")]
		if t == nil {
			problem(w, 404, client.CodeTagNotFound, "tag not found")
			return
		}
		var body named
		if !decode(w, r, &body) {
			return
		}
		if body.Name != nil {
			t.Name = *body.Name
		}
		if body.Description != nil {
			t.Description = desc(body.Description)
		}
		reply(w, 200, t)
	})
	mux.HandleFunc("DELETE /orgs/{org}/tags/{id}", func(w http.ResponseWriter, r *http.Request) {
		if f.tags[r.PathValue("id")] == nil {
			problem(w, 404, client.CodeTagNotFound, "tag not found")
			return
		}
		delete(f.tags, r.PathValue("id"))
		w.WriteHeader(204)
	})

	// Routes, kept in order.
	route := func(id string) *client.Route {
		for _, rt := range f.routes {
			if rt.ID == id {
				return rt
			}
		}
		return nil
	}
	list := func() map[string]any {
		for i, rt := range f.routes {
			rt.Position = int64(i + 1)
		}
		return map[string]any{"items": f.routes}
	}
	mux.HandleFunc("GET /orgs/{org}/routes", func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, list())
	})
	mux.HandleFunc("POST /orgs/{org}/routes", func(w http.ResponseWriter, r *http.Request) {
		var rt client.Route
		if !decode(w, r, &rt) {
			return
		}
		now := time.Now().UTC()
		rt.ID, rt.CreatedAt, rt.UpdatedAt = f.id(), now, now
		rt.AllowGroupIDs = lowerIDs(rt.AllowGroupIDs)
		rt.AllowTagIDs = lowerIDs(rt.AllowTagIDs)
		rt.Note = desc(rt.Note)
		f.routes = append(f.routes, &rt)
		list()
		reply(w, 201, &rt)
	})
	mux.HandleFunc("PUT /orgs/{org}/routes/order", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			RouteIDs []string `json:"route_ids"`
		}
		if !decode(w, r, &body) {
			return
		}
		if len(body.RouteIDs) != len(f.routes) {
			problem(w, 409, "org.routes_changed", "the rules changed meanwhile; reload them and try again")
			return
		}
		ordered := make([]*client.Route, 0, len(f.routes))
		for _, id := range lowerIDs(body.RouteIDs) {
			rt := route(id)
			if rt == nil {
				problem(w, 409, "org.routes_changed", "the rules changed meanwhile; reload them and try again")
				return
			}
			ordered = append(ordered, rt)
		}
		f.routes = ordered
		reply(w, 200, list())
	})
	mux.HandleFunc("GET /orgs/{org}/routes/{id}", func(w http.ResponseWriter, r *http.Request) {
		rt := route(r.PathValue("id"))
		if rt == nil {
			problem(w, 404, client.CodeRouteNotFound, "rule not found")
			return
		}
		reply(w, 200, rt)
	})
	mux.HandleFunc("PATCH /orgs/{org}/routes/{id}", func(w http.ResponseWriter, r *http.Request) {
		rt := route(r.PathValue("id"))
		if rt == nil {
			problem(w, 404, client.CodeRouteNotFound, "rule not found")
			return
		}
		var body struct {
			SlugPattern   *string   `json:"slug_pattern"`
			AppliesTo     *string   `json:"applies_to"`
			AllowGroupIDs *[]string `json:"allow_group_ids"`
			AllowTagIDs   *[]string `json:"allow_tag_ids"`
			Note          *string   `json:"note"`
		}
		if !decode(w, r, &body) {
			return
		}
		if body.SlugPattern != nil {
			rt.SlugPattern = *body.SlugPattern
		}
		if body.AppliesTo != nil {
			rt.AppliesTo = *body.AppliesTo
		}
		if body.AllowGroupIDs != nil {
			rt.AllowGroupIDs = lowerIDs(*body.AllowGroupIDs)
		}
		if body.AllowTagIDs != nil {
			rt.AllowTagIDs = lowerIDs(*body.AllowTagIDs)
		}
		if body.Note != nil {
			rt.Note = desc(body.Note)
		}
		rt.UpdatedAt = time.Now().UTC()
		reply(w, 200, rt)
	})
	mux.HandleFunc("DELETE /orgs/{org}/routes/{id}", func(w http.ResponseWriter, r *http.Request) {
		if route(r.PathValue("id")) == nil {
			problem(w, 404, client.CodeRouteNotFound, "rule not found")
			return
		}
		f.routes = slices.DeleteFunc(f.routes, func(rt *client.Route) bool { return rt.ID == r.PathValue("id") })
		w.WriteHeader(204)
	})

	// Keys.
	mux.HandleFunc("POST /orgs/{org}/keys", func(w http.ResponseWriter, r *http.Request) {
		var k client.Key
		if !decode(w, r, &k) {
			return
		}
		if k.Permissions == (client.KeyPermissions{}) {
			k.Permissions = client.KeyPermissions{Activities: "manage", Notifications: "schedule", Widgets: "none", Emails: "none"}
		}
		if k.Target != nil && k.Permissions.Emails != "none" {
			problem(w, 422, "org.key_permission_unavailable", "a key limited to groups and tags cannot send emails")
			return
		}
		if k.Target != nil {
			k.Target.GroupIDs, k.Target.TagIDs = lowerIDs(k.Target.GroupIDs), lowerIDs(k.Target.TagIDs)
			if len(k.Target.GroupIDs)+len(k.Target.TagIDs) == 0 {
				problem(w, 422, "org.routing_invalid", "target must name at least one group or tag; omit it to let the key send to everyone")
				return
			}
		}
		if k.ExpiresAt != nil && !k.ExpiresAt.After(time.Now()) {
			problem(w, 400, "integration_key.expires_at_invalid", "expires_at must be in the future")
			return
		}
		if k.ExpiresAt != nil {
			utc := k.ExpiresAt.UTC()
			k.ExpiresAt = &utc
		}
		k.ID, k.CreatedAt = f.id(), time.Now().UTC()
		if len(k.ActivitySlugs) == 0 {
			k.ActivitySlugs = nil
		}
		if len(k.WidgetSlugs) == 0 {
			k.WidgetSlugs = nil
		}
		stored := k
		secret := "hlk_" + strings.ReplaceAll(k.ID, "-", "")
		k.Secret = &secret
		f.keys[k.ID] = &stored
		reply(w, 201, k)
	})
	mux.HandleFunc("GET /orgs/{org}/keys/{id}", func(w http.ResponseWriter, r *http.Request) {
		k := f.keys[r.PathValue("id")]
		if k == nil {
			problem(w, 404, client.CodeKeyNotFound, "integration key not found")
			return
		}
		reply(w, 200, k)
	})
	mux.HandleFunc("PATCH /orgs/{org}/keys/{id}", func(w http.ResponseWriter, r *http.Request) {
		k := f.keys[r.PathValue("id")]
		if k == nil {
			problem(w, 404, client.CodeKeyNotFound, "integration key not found")
			return
		}
		var body map[string]json.RawMessage
		if !decode(w, r, &body) {
			return
		}
		if _, ok := body["target"]; ok {
			problem(w, 422, "", "target cannot change")
			return
		}
		if raw, ok := body["permissions"]; ok && k.Target != nil {
			var p client.KeyPermissions
			_ = json.Unmarshal(raw, &p)
			if p.Emails != "" && p.Emails != "none" {
				problem(w, 422, "org.key_permission_unavailable", "a key limited to groups and tags cannot send emails")
				return
			}
		}
		if raw, ok := body["name"]; ok {
			_ = json.Unmarshal(raw, &k.Name)
		}
		if raw, ok := body["permissions"]; ok {
			_ = json.Unmarshal(raw, &k.Permissions)
		}
		for field, dst := range map[string]*[]string{"activity_slugs": &k.ActivitySlugs, "widget_slugs": &k.WidgetSlugs} {
			if raw, ok := body[field]; ok {
				*dst = nil
				_ = json.Unmarshal(raw, dst)
				if len(*dst) == 0 {
					*dst = nil
				}
			}
		}
		if raw, ok := body["expires_at"]; ok {
			var exp *time.Time
			if err := json.Unmarshal(raw, &exp); err != nil || (exp != nil && !exp.After(time.Now())) {
				problem(w, 400, "integration_key.expires_at_invalid", "expires_at must be in the future")
				return
			}
			if exp != nil {
				utc := exp.UTC()
				exp = &utc
			}
			k.ExpiresAt = exp
		}
		reply(w, 200, k)
	})
	mux.HandleFunc("DELETE /orgs/{org}/keys/{id}", func(w http.ResponseWriter, r *http.Request) {
		if f.keys[r.PathValue("id")] == nil {
			problem(w, 404, client.CodeKeyNotFound, "integration key not found")
			return
		}
		delete(f.keys, r.PathValue("id"))
		w.WriteHeader(204)
	})
}
