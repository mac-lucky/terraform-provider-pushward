package provider

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/mac-lucky/terraform-provider-pushward/internal/client"
)

// apiSpec is the part of the public OpenAPI spec the tests hold the fake API
// and the client types to.
type apiSpec struct {
	Paths      map[string]map[string]*specOp `yaml:"paths"`
	Components struct {
		Schemas map[string]*specSchema `yaml:"schemas"`
	} `yaml:"components"`

	// ops is every operation by "METHOD /path" with the path parameters
	// written {}, so that the fake's {org} matches the spec's {org_id}.
	ops map[string]*specOp
}

type specContent map[string]struct {
	Schema *specSchema `yaml:"schema"`
}

type specOp struct {
	OperationID string `yaml:"operationId"`
	Description string `yaml:"description"`
	RequestBody *struct {
		Content specContent `yaml:"content"`
	} `yaml:"requestBody"`
	Responses map[string]struct {
		Content specContent `yaml:"content"`
	} `yaml:"responses"`
}

type specSchema struct {
	Ref                  string                 `yaml:"$ref"`
	Properties           map[string]*specSchema `yaml:"properties"`
	Items                *specSchema            `yaml:"items"`
	AdditionalProperties any                    `yaml:"additionalProperties"`
}

var pathParam = regexp.MustCompile(`\{[^}]*\}`)

func opKey(method, path string) string {
	return strings.ToUpper(method) + " " + pathParam.ReplaceAllString(path, "{}")
}

// loadSpec parses testdata/openapi.yaml, once. PUSHWARD_SPEC_URL, such as
// https://api.pushward.app/openapi.yaml, runs the tests against that spec
// instead.
var loadSpec = sync.OnceValues(func() (*apiSpec, error) {
	var data []byte
	var err error
	if u := os.Getenv("PUSHWARD_SPEC_URL"); u != "" {
		var resp *http.Response
		if resp, err = http.Get(u); err != nil { // #nosec G107 -- the developer chooses the spec URL
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GET %s: %s", u, resp.Status)
		}
		data, err = io.ReadAll(resp.Body)
	} else {
		data, err = os.ReadFile("testdata/openapi.yaml")
	}
	if err != nil {
		return nil, err
	}
	s := &apiSpec{ops: map[string]*specOp{}}
	if err := yaml.Unmarshal(data, s); err != nil {
		return nil, err
	}
	for path, methods := range s.Paths {
		for method, op := range methods {
			s.ops[opKey(method, path)] = op
		}
	}
	return s, nil
})

func testSpec(t *testing.T) *apiSpec {
	t.Helper()
	s, err := loadSpec()
	if err != nil {
		t.Fatalf("loading the OpenAPI spec: %v", err)
	}
	return s
}

// op is the operation of a ServeMux pattern such as "GET /orgs/{org}".
func (s *apiSpec) op(pattern string) *specOp {
	method, path, _ := strings.Cut(pattern, " ")
	return s.ops[opKey(method, path)]
}

func (s *apiSpec) byID(id string) *specOp {
	for _, op := range s.ops {
		if op.OperationID == id {
			return op
		}
	}
	return nil
}

func (s *apiSpec) resolve(sc *specSchema) *specSchema {
	for sc != nil && sc.Ref != "" {
		sc = s.Components.Schemas[strings.TrimPrefix(sc.Ref, "#/components/schemas/")]
	}
	return sc
}

func (op *specOp) request() *specSchema {
	if op.RequestBody == nil {
		return nil
	}
	return op.RequestBody.Content["application/json"].Schema
}

func (op *specOp) response() *specSchema {
	for _, code := range []string{"200", "201"} {
		if r, ok := op.Responses[code]; ok {
			return r.Content["application/json"].Schema
		}
	}
	return nil
}

// unknownKeys lists the keys in v, at any depth, that sc does not have and
// does not allow (additionalProperties: false).
func (s *apiSpec) unknownKeys(sc *specSchema, v any, at string) []string {
	if sc = s.resolve(sc); sc == nil {
		return nil
	}
	var out []string
	switch v := v.(type) {
	case map[string]any:
		for k, sub := range v {
			if p, ok := sc.Properties[k]; ok {
				out = append(out, s.unknownKeys(p, sub, at+"."+k)...)
			} else if sc.AdditionalProperties == false {
				out = append(out, at+"."+k)
			}
		}
	case []any:
		for i, e := range v {
			out = append(out, s.unknownKeys(sc.Items, e, fmt.Sprintf("%s[%d]", at, i))...)
		}
	}
	slices.Sort(out)
	return out
}

// TestFakeRoutesInSpec holds the fake to the public spec: every route it
// serves is an operation there, one an org admin key may call, so the
// provider is never tested against a route the API does not have.
func TestFakeRoutesInSpec(t *testing.T) {
	spec := testSpec(t)
	f := newFake(t)
	if len(f.patterns) == 0 {
		t.Fatal("the fake recorded no routes")
	}
	for _, p := range f.patterns {
		op := spec.op(p)
		if op == nil {
			t.Errorf("the fake serves %s, which is not in the spec", p)
			continue
		}
		if !strings.Contains(op.Description, "Takes an org admin key (hlo_)") {
			t.Errorf("%s (%s) does not say it takes an org admin key: %q", p, op.OperationID, op.Description)
		}
	}
}

// TestClientTypesMatchSpec holds every json tag of the client types to the
// properties of the response the provider decodes them from.
func TestClientTypesMatchSpec(t *testing.T) {
	spec := testSpec(t)
	response := func(id string) *specSchema {
		op := spec.byID(id)
		if op == nil || op.response() == nil {
			t.Fatalf("the spec has no operation %s with a JSON response", id)
		}
		return op.response()
	}
	devices := spec.resolve(response("listOrgDevices")).Properties["items"]
	if devices == nil || devices.Items == nil {
		t.Fatal("listOrgDevices has no items")
	}
	for _, tc := range []struct {
		v       any
		schemas []*specSchema
	}{
		{client.Org{}, []*specSchema{response("getOrg")}},
		{client.Member{}, []*specSchema{response("getOrgMember")}},
		{client.Invite{}, []*specSchema{response("getOrgInvite")}},
		{client.Group{}, []*specSchema{response("getOrgGroup")}},
		{client.Tag{}, []*specSchema{response("getOrgTag")}},
		{client.Route{}, []*specSchema{response("getOrgRoute")}},
		// The secret, key, is only in the responses that create or roll one.
		{client.Key{}, []*specSchema{response("getOrgKey"), response("createOrgKey")}},
		{client.Device{}, []*specSchema{devices.Items}},
	} {
		typ := reflect.TypeOf(tc.v)
		for _, msg := range spec.checkTags(typ, tc.schemas, typ.Name()) {
			t.Error(msg)
		}
	}
}

// checkTags lists typ's json tags that none of schemas has as a property. A
// field of struct type is checked against the property's own schema.
func (s *apiSpec) checkTags(typ reflect.Type, schemas []*specSchema, at string) []string {
	var out []string
	for i := range typ.NumField() {
		field := typ.Field(i)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		var props []*specSchema
		for _, sc := range schemas {
			if sc = s.resolve(sc); sc != nil && sc.Properties[name] != nil {
				props = append(props, s.resolve(sc.Properties[name]))
			}
		}
		if len(props) == 0 {
			out = append(out, fmt.Sprintf("%s.%s: the spec's response has no %q", at, field.Name, name))
			continue
		}
		ft := field.Type
		for ft.Kind() == reflect.Pointer || ft.Kind() == reflect.Slice {
			ft = ft.Elem()
		}
		if ft.Kind() != reflect.Struct || ft == reflect.TypeFor[time.Time]() {
			continue
		}
		for j, p := range props {
			if p != nil && p.Items != nil {
				props[j] = p.Items
			}
		}
		out = append(out, s.checkTags(ft, props, at+"."+field.Name)...)
	}
	return out
}

// The checks above must be able to fail.
func TestSpecChecksCatchDrift(t *testing.T) {
	spec := testSpec(t)
	body := map[string]any{"add": []any{map[string]any{"user_id": "u", "device_id": "d", "tag": "x"}}, "replace": true}
	got := spec.unknownKeys(spec.op("POST /orgs/{org}/tags/{tag}/devices").request(), body, "body")
	if want := []string{"body.add[0].tag", "body.replace"}; !slices.Equal(got, want) {
		t.Errorf("unknownKeys = %v, want %v", got, want)
	}
	typo := reflect.TypeFor[struct {
		Nickname string `json:"nickname"`
		Target   *struct {
			GroupIDs []string `json:"group_ids"`
			Typo     []string `json:"group_idz"`
		} `json:"target"`
	}]()
	if got := spec.checkTags(typo, []*specSchema{spec.byID("getOrgKey").response()}, "x"); len(got) != 2 {
		t.Errorf("checkTags = %q, want nickname and target.group_idz", got)
	}
}
